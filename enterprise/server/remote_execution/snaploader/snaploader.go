package snaploader

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/buildbuddy-io/buildbuddy/enterprise/server/remote_execution/copy_on_write"
	"github.com/buildbuddy-io/buildbuddy/enterprise/server/util/filecacheutil"
	"github.com/buildbuddy-io/buildbuddy/server/environment"
	"github.com/buildbuddy-io/buildbuddy/server/metrics"
	"github.com/buildbuddy-io/buildbuddy/server/remote_cache/digest"
	"github.com/buildbuddy-io/buildbuddy/server/util/authutil"
	"github.com/buildbuddy-io/buildbuddy/server/util/hash"
	"github.com/buildbuddy-io/buildbuddy/server/util/log"
	"github.com/buildbuddy-io/buildbuddy/server/util/perms"
	"github.com/buildbuddy-io/buildbuddy/server/util/status"
	"github.com/buildbuddy-io/buildbuddy/server/util/tracing"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	fcpb "github.com/buildbuddy-io/buildbuddy/proto/firecracker"
	repb "github.com/buildbuddy-io/buildbuddy/proto/remote_execution"
)

var EnableLocalSnapshotSharing = flag.Bool("executor.enable_local_snapshot_sharing", false, "Enables local snapshot sharing for firecracker VMs. Also requires that executor.firecracker_enable_nbd is true.")

const (
	// File name used for the rootfs snapshot artifact.
	rootfsFileName = "rootfs.ext4"
)

// NewKey returns the cache key for a snapshot.
// TODO: include a version number in the key somehow, so that
// if we make breaking changes e.g. to the vmexec API or firecracker
// version etc., we can ensure that incompatible snapshots don't get reused.
func NewKey(task *repb.ExecutionTask, configurationHash, runnerID string) (*fcpb.SnapshotKey, error) {
	pd, err := digest.ComputeForMessage(task.GetCommand().GetPlatform(), repb.DigestFunction_SHA256)
	if err != nil {
		return nil, status.WrapErrorf(err, "failed to compute platform hash")
	}
	return &fcpb.SnapshotKey{
		InstanceName:      task.GetExecuteRequest().GetInstanceName(),
		PlatformHash:      pd.GetHash(),
		ConfigurationHash: configurationHash,
		RunnerId:          runnerID,
	}, nil
}

// manifestFileCacheKey returns the filecache key for the snapshot manifest
// file.
//
// We always want runners to use the newest manifest (and corresponding
// snapshot), so they should overwrite any existing manifest when saving
// snapshots so that newer runners will read from the newer version
func manifestFileCacheKey(ctx context.Context, env environment.Env, s *fcpb.SnapshotKey) *repb.FileNode {
	// Note: .manifest is not a real file that we ever create on disk, it's
	// effectively just part of the cache key used to locate the manifest.
	key, _ := artifactFileCacheKey(ctx, env, false, s, ".manifest", 1 /*=arbitrary size*/)
	return key
}

// artifactFileCacheKey returns the cache key for a snapshot artifact.
// It reads the artifact using fileReader in order to compute a digest
// of its contents
//
// If you don't need a real digest - for example because computing digests
// of large snapshot files is expensive -  pass in a nil fileReader.
// This will return a hash of the file name and snapshot key instead
func artifactFileCacheKey(ctx context.Context, env environment.Env, computeDigest bool, s *fcpb.SnapshotKey, filePath string, sizeBytes int64) (*repb.FileNode, error) {
	if computeDigest {
		// TODO(Maggie): Add metrics for computing snapshot digests
		file, err := os.Open(filePath)
		if err != nil {
			return nil, err
		}
		defer file.Close()

		fileReader := bufio.NewReader(file)
		d, err := digest.Compute(fileReader, repb.DigestFunction_BLAKE3)
		if err != nil {
			return nil, err
		}
		return &repb.FileNode{
			Digest: d,
		}, nil
	}
	fileName := filepath.Base(filePath)
	gid, err := groupID(ctx, env)
	if err != nil {
		return nil, err
	}
	// Note that this only works because filecache doesn't
	// verify digests. If you want to store these remotely in
	// CAS, you need to compute the full digest.
	return &repb.FileNode{
		Digest: &repb.Digest{
			Hash:      hashStrings(gid, s.InstanceName, s.PlatformHash, s.ConfigurationHash, s.RunnerId, fileName),
			SizeBytes: sizeBytes,
		},
	}, nil
}

// Snapshot holds a snapshot manifest along with the corresponding cache key.
type Snapshot struct {
	key      *fcpb.SnapshotKey
	manifest *fcpb.SnapshotManifest
}

func (s *Snapshot) GetVMConfiguration() *fcpb.VMConfiguration {
	return s.manifest.GetVmConfiguration()
}

// CacheSnapshotOptions contains any assets or configuration to be associated
// with a stored snapshot.
//
// All fields are optional, as snapshots may represent different things, such as
// an asset shared across VMs (such as the containerfs), or a fully snapshotted
// VM.
type CacheSnapshotOptions struct {
	VMConfiguration     *fcpb.VMConfiguration
	VMStateSnapshotPath string
	KernelImagePath     string
	InitrdImagePath     string
	MemSnapshotPath     string

	// TODO: remove these 3 in favor of a single rootfs.
	ContainerFSPath string
	ScratchFSPath   string
	WorkspaceFSPath string

	// Labeled map of chunked artifacts backed by copy_on_write.COWStore storage.
	ChunkedFiles map[string]*copy_on_write.COWStore
}

type UnpackedSnapshot struct {
	// ChunkedFiles holds any chunked files that were part of the snapshot.
	ChunkedFiles map[string]*copy_on_write.COWStore
}

func enumerateFiles(snapOpts *CacheSnapshotOptions) []string {
	var out []string
	for _, p := range []string{
		snapOpts.VMStateSnapshotPath,
		snapOpts.KernelImagePath,
		snapOpts.InitrdImagePath,
		snapOpts.MemSnapshotPath,
		snapOpts.ContainerFSPath,
		snapOpts.ScratchFSPath,
		snapOpts.WorkspaceFSPath,
	} {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Loader loads and stores snapshot artifacts to cache. Only a single loader
// instance is required - the loader is stateless and loader operations can be
// used concurrently by different snapshots.
type Loader interface {
	// CacheSnapshot saves a local snapshot with the given key to cache, with the
	// snapshot configuration and artifact paths specified by opts.
	CacheSnapshot(ctx context.Context, key *fcpb.SnapshotKey, opts *CacheSnapshotOptions) (*Snapshot, error)

	// GetSnapshot loads the metadata for the snapshot. It does not
	// unpack any snapshot artifacts.
	// It returns UnavailableError if the metadata has expired from cache.
	GetSnapshot(ctx context.Context, key *fcpb.SnapshotKey) (*Snapshot, error)

	// UnpackSnapshot unpacks a snapshot to the given directory.
	// It returns UnavailableError if any snapshot artifacts have expired
	// from cache.
	UnpackSnapshot(ctx context.Context, snapshot *Snapshot, outputDirectory string) (*UnpackedSnapshot, error)

	// DeleteSnapshot removes the snapshot artifacts from cache
	// as well as the manifest entry.
	// This is useful to free up cache space used by stale snapshots.
	// Snapshots are quite large (tens of GB) so a single VM being
	// paused and resumed can cause significant cache churn.
	DeleteSnapshot(ctx context.Context, snapshot *Snapshot) error
}

type FileCacheLoader struct {
	env environment.Env
}

func New(env environment.Env) (*FileCacheLoader, error) {
	if env.GetFileCache() == nil {
		return nil, status.InvalidArgumentError("missing FileCache in env")
	}
	return &FileCacheLoader{env: env}, nil
}

func (l *FileCacheLoader) GetSnapshot(ctx context.Context, key *fcpb.SnapshotKey) (*Snapshot, error) {
	manifestNode := manifestFileCacheKey(ctx, l.env, key)
	buf, err := filecacheutil.Read(l.env.GetFileCache(), manifestNode)
	if err != nil {
		return nil, status.UnavailableErrorf("failed to read snapshot manifest: %s", status.Message(err))
	}
	manifest := &fcpb.SnapshotManifest{}
	if err := proto.Unmarshal(buf, manifest); err != nil {
		return nil, status.UnavailableErrorf("failed to unmarshal snapshot manifest: %s", status.Message(err))
	}

	// Check whether all artifacts in the manifest are available. This helps
	// make sure that the snapshot we return can actually be loaded. This also
	// updates the last access time of all the artifacts, which helps prevent
	// the snapshot artifacts from expiring just after we've returned it.
	if err := l.checkAllArtifactsExist(ctx, manifest); err != nil {
		return nil, err
	}

	return &Snapshot{key: key, manifest: manifest}, nil
}

func (l *FileCacheLoader) UnpackSnapshot(ctx context.Context, snapshot *Snapshot, outputDirectory string) (*UnpackedSnapshot, error) {
	if snapshot == nil {
		return nil, status.InvalidArgumentErrorf("no snapshot to unpack")
	}

	for _, fileNode := range snapshot.manifest.Files {
		if !l.env.GetFileCache().FastLinkFile(fileNode, filepath.Join(outputDirectory, fileNode.GetName())) {
			return nil, status.UnavailableErrorf("snapshot artifact %q not found in local cache", fileNode.GetName())
		}
	}

	unpacked := &UnpackedSnapshot{
		ChunkedFiles: make(map[string]*copy_on_write.COWStore, len(snapshot.manifest.ChunkedFiles)),
	}
	// Construct COWs from chunks.
	for _, cf := range snapshot.manifest.ChunkedFiles {
		cow, err := l.unpackCOW(ctx, cf, outputDirectory)
		if err != nil {
			return nil, status.WrapError(err, "unpack COW")
		}
		unpacked.ChunkedFiles[cf.GetName()] = cow
	}

	return unpacked, nil
}

func (l *FileCacheLoader) DeleteSnapshot(ctx context.Context, snapshot *Snapshot) error {
	// Manually evict the manifest as well as all referenced files.
	l.env.GetFileCache().DeleteFile(manifestFileCacheKey(ctx, l.env, snapshot.key))
	for _, fileNode := range snapshot.manifest.Files {
		l.env.GetFileCache().DeleteFile(fileNode)
	}
	return nil
}

func (l *FileCacheLoader) CacheSnapshot(ctx context.Context, key *fcpb.SnapshotKey, opts *CacheSnapshotOptions) (*Snapshot, error) {
	manifest := &fcpb.SnapshotManifest{
		VmConfiguration: opts.VMConfiguration,
	}
	// Put the files from the snapshot into the filecache and record their
	// names and digests in the manifest so they can be unpacked later.
	for _, f := range enumerateFiles(opts) {
		info, err := os.Stat(f)
		if err != nil {
			return nil, err
		}
		// If snapshot sharing is disabled, don't compute the digest for the
		// file because it is costly. Because the runner ID is in the key
		// when snapshot sharing is disabled,  we don't need to worry about
		// multiple runners trying to access the same key simultaneously
		fileNode, err := artifactFileCacheKey(ctx, l.env, *EnableLocalSnapshotSharing, key, f, info.Size())
		if err != nil {
			return nil, err
		}
		fileNode.Name = filepath.Base(f)
		manifest.Files = append(manifest.Files, fileNode)

		// If EnableLocalSnapshotSharing=true and we're computing real digests,
		// the files will be immutable. We won't need to re-save them to file cache
		if !*EnableLocalSnapshotSharing || !l.env.GetFileCache().ContainsFile(fileNode) {
			l.env.GetFileCache().AddFile(fileNode, f)
		}
	}
	for name, cow := range opts.ChunkedFiles {
		cf, err := l.cacheCOW(ctx, name, cow)
		if err != nil {
			return nil, status.WrapErrorf(err, "cache %q COW", name)
		}
		manifest.ChunkedFiles = append(manifest.ChunkedFiles, cf)
	}
	// Write the manifest file and put it in the filecache too. We'll
	// retrieve this later in order to unpack the snapshot.
	b, err := proto.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	manifestNode := manifestFileCacheKey(ctx, l.env, key)
	if _, err := filecacheutil.Write(l.env.GetFileCache(), manifestNode, b); err != nil {
		return nil, err
	}
	return &Snapshot{key: key, manifest: manifest}, nil
}

func (l *FileCacheLoader) checkAllArtifactsExist(ctx context.Context, manifest *fcpb.SnapshotManifest) error {
	for _, f := range manifest.GetFiles() {
		if !l.env.GetFileCache().ContainsFile(f) {
			return status.NotFoundErrorf("file %q not found (digest %q)", f.GetName(), digest.String(f.GetDigest()))
		}
	}
	for _, cf := range manifest.GetChunkedFiles() {
		for _, c := range cf.GetChunks() {
			node := &repb.FileNode{
				Digest: &repb.Digest{
					Hash:      c.GetDigestHash(),
					SizeBytes: chunkDigestSize(cf, c),
				},
			}
			if !l.env.GetFileCache().ContainsFile(node) {
				return status.NotFoundErrorf("chunked file %q missing chunk at offset 0x%x (digest %q)", cf.GetName(), c.GetOffset(), digest.String(node.Digest))
			}
		}
	}
	return nil
}

func (l *FileCacheLoader) unpackCOW(ctx context.Context, file *fcpb.ChunkedFile, outputDirectory string) (cf *copy_on_write.COWStore, err error) {
	dataDir := filepath.Join(outputDirectory, file.GetName())
	if err := os.Mkdir(dataDir, 0755); err != nil {
		return nil, status.InternalErrorf("failed to create COW data dir %q: %s", dataDir, err)
	}
	var chunks []*copy_on_write.Mmap
	defer func() {
		// If there was an error, clean up any chunks we created.
		if err == nil {
			return
		}
		for _, c := range chunks {
			c.Close()
		}
	}()
	for _, chunk := range file.Chunks {
		size := file.GetChunkSize()
		if remainder := file.GetSize() - chunk.GetOffset(); size > remainder {
			size = remainder
		}
		d := &repb.Digest{Hash: chunk.GetDigestHash(), SizeBytes: size}
		node := &repb.FileNode{Digest: d}
		path := filepath.Join(dataDir, fmt.Sprintf("%d", chunk.GetOffset()))
		if !l.env.GetFileCache().FastLinkFile(node, path) {
			return nil, status.UnavailableErrorf("snapshot chunk %s/%d not found in local cache", file.GetName(), chunk.GetOffset())
		}
		c, err := copy_on_write.NewLazyMmap(path, chunk.GetOffset())
		if err != nil {
			return nil, status.WrapError(err, "create mmap for chunk")
		}
		// Memoize the original digest so that if the chunk doesn't change we
		// don't have to recompute it later when adding back to cache.
		c.SetDigest(d)
		chunks = append(chunks, c)
	}
	cow, err := copy_on_write.NewCOWStore(chunks, file.GetChunkSize(), file.GetSize(), dataDir)
	if err != nil {
		return nil, err
	}
	return cow, nil
}

func (l *FileCacheLoader) cacheCOW(ctx context.Context, name string, cow *copy_on_write.COWStore) (*fcpb.ChunkedFile, error) {
	size, err := cow.SizeBytes()
	if err != nil {
		return nil, err
	}
	pb := &fcpb.ChunkedFile{
		Name:      name,
		Size:      size,
		ChunkSize: cow.ChunkSizeBytes(),
	}
	dirtyChunkCount := 0
	var dirtyBytes int64
	chunks := cow.SortedChunks()
	for _, c := range chunks {
		if cow.Dirty(c.Offset) {
			dirtyChunkCount++
			chunkSize, err := c.SizeBytes()
			if err != nil {
				return nil, status.WrapError(err, "dirty chunk size")
			}
			dirtyBytes += chunkSize

			// Sync dirty chunks to make sure the underlying file is up to date
			// before we add it to cache.
			if err := c.Sync(); err != nil {
				return nil, status.WrapError(err, "sync dirty chunk")
			}
		}
		d, err := c.Digest()
		if err != nil {
			return nil, err
		}
		node := &repb.FileNode{Digest: d}
		path := filepath.Join(cow.DataDir(), cow.ChunkName(c.Offset))
		// TODO: if the file is already cached, then instead of adding the file,
		// just record a file access (to avoid the syscall overhead of
		// unlink/relink).
		l.env.GetFileCache().AddFile(node, path)
		pb.Chunks = append(pb.Chunks, &fcpb.Chunk{
			Offset:     c.Offset,
			DigestHash: d.GetHash(),
		})
	}

	gid, err := groupID(ctx, l.env)
	if err != nil {
		return nil, err
	}
	metrics.COWSnapshotDirtyChunkRatio.With(prometheus.Labels{
		metrics.GroupID:  gid,
		metrics.FileName: name,
	}).Observe(float64(dirtyChunkCount) / float64(len(chunks)))
	metrics.COWSnapshotDirtyBytes.With(prometheus.Labels{
		metrics.GroupID:  gid,
		metrics.FileName: name,
	}).Add(float64(dirtyBytes))

	return pb, nil
}

func chunkDigestSize(chunkedFile *fcpb.ChunkedFile, chunk *fcpb.Chunk) int64 {
	size := chunkedFile.GetChunkSize()
	if remainder := chunkedFile.GetSize() - chunk.GetOffset(); remainder < size {
		size = remainder
	}
	return size
}

func hashStrings(strs ...string) string {
	out := ""
	for _, s := range strs {
		out += hash.String(s)
	}
	return hash.String(out)
}

func groupID(ctx context.Context, env environment.Env) (string, error) {
	var gid string
	u, err := perms.AuthenticatedUser(ctx, env)
	if err == nil {
		gid = u.GetGroupID()
	} else if err != nil && !authutil.IsAnonymousUserError(err) {
		return "", err
	}
	return gid, nil
}

// UnpackContainerImage returns a ChunkedFile representing the given container
// image. The chunk dir is stored as a child directory of the given outDir.
//
// If the image is not cached, this func will split up the given ext4 image
// file and create a new ChunkedFile from it, then add that to cache.
func UnpackContainerImage(ctx context.Context, l *FileCacheLoader, imageRef, imageExt4Path string, outDir string, chunkSize int64) (*copy_on_write.COWStore, error) {
	ctx, span := tracing.StartSpan(ctx)
	defer span.End()

	// TODO: use an Action for this key instead (to allow remote snapshot
	// sharing).
	key := &fcpb.SnapshotKey{
		ConfigurationHash: hashStrings("__UnpackContainerImage", imageRef),
	}

	snap, err := l.GetSnapshot(ctx, key)
	if err != nil && !(status.IsNotFoundError(err) || status.IsUnavailableError(err)) {
		return nil, err
	}
	if snap != nil {
		unpacked, err := l.UnpackSnapshot(ctx, snap, outDir)
		if err != nil {
			return nil, err
		}
		cf := unpacked.ChunkedFiles[rootfsFileName]
		if cf == nil {
			return nil, status.InternalError("missing rootfs artifact in snapshot")
		}
		return cf, nil
	}
	// containerfs is not available in cache; convert the EXT4 image to a
	// ChunkedFile then add it to cache.
	// TODO(bduffany): single-flight this.
	start := time.Now()
	cow, err := copy_on_write.ConvertFileToCOW(imageExt4Path, chunkSize, outDir)
	if err != nil {
		return nil, status.WrapError(err, "convert image to COW")
	}
	// Add the COW to cache. This will also compute chunk digests.
	opts := &CacheSnapshotOptions{
		ChunkedFiles: map[string]*copy_on_write.COWStore{rootfsFileName: cow},
	}
	if _, err := l.CacheSnapshot(ctx, key, opts); err != nil {
		return nil, status.WrapError(err, "cache containerfs snapshot")
	}
	log.CtxDebugf(ctx, "Converted containerfs to COW in %s", time.Since(start))
	return cow, nil
}
