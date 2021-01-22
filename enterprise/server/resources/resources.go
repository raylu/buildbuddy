package resources

import (
	"flag"
	"log"
	"os"
	"runtime"
	"strconv"
	"sync"

	"github.com/elastic/gosigar"
)

const (
	memoryEnvVarName   = "SYS_MEMORY_BYTES"
	cpuEnvVarName      = "SYS_MILLICPU"
	nodeEnvVarName     = "MY_NODENAME"
	hostnameEnvVarName = "MY_HOSTNAME"
	portEnvVarName     = "MY_PORT"
	poolEnvVarName     = "MY_POOL"
)

var (
	allocatedRAMBytes  int64
	allocatedCPUMillis int64
	once               sync.Once
)

func init() {
	once.Do(func() {
		setSysRAMBytes()
		setSysMilliCPUCapacity()
	})
}

func setSysRAMBytes() {
	if v := os.Getenv(memoryEnvVarName); v != "" {
		i, err := strconv.ParseInt(v, 10, 64)
		if err == nil {
			allocatedRAMBytes = i
			return
		}
	}
	mem := gosigar.Mem{}
	mem.Get()
	allocatedRAMBytes = int64(mem.ActualFree)
	log.Printf("set allocatedRAMBytes to %d", allocatedRAMBytes)
}

func setSysMilliCPUCapacity() {
	if v := os.Getenv(cpuEnvVarName); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err == nil {
			allocatedCPUMillis = int64(f * 1000)
			return
		}
	}

	cpuList := gosigar.CpuList{}
	cpuList.Get()
	numCores := len(cpuList.List)
	allocatedCPUMillis = int64(numCores * 1000)
	log.Printf("set allocatedCPUMillis to %d", allocatedCPUMillis)
}

func GetSysFreeRAMBytes() int64 {
	mem := gosigar.Mem{}
	mem.Get()
	return int64(mem.ActualFree)
}

func GetAllocatedRAMBytes() int64 {
	return allocatedRAMBytes
}

func GetAllocatedCPUMillis() int64 {
	return allocatedCPUMillis
}

func GetNodeName() string {
	return os.Getenv(nodeEnvVarName)
}

func GetPoolName() string {
	return os.Getenv(poolEnvVarName)
}

func GetArch() string {
	return runtime.GOARCH
}

func GetOS() string {
	return runtime.GOOS
}

func GetMyHostname() (string, error) {
	if v := os.Getenv(hostnameEnvVarName); v != "" {
		return v, nil
	}
	return os.Hostname()
}

func GetMyPort() (int32, error) {
	portStr := ""
	if v := os.Getenv(portEnvVarName); v != "" {
		portStr = v
	} else {
		if v := flag.Lookup("grpc_port"); v != nil {
			portStr = v.Value.String()
		}
	}
	i, err := strconv.ParseInt(portStr, 10, 32)
	if err != nil {
		return 0, err
	}
	return int32(i), nil
}
