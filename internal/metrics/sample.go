// Package metrics collects a sample of host metrics from /proc and statfs.
package metrics

// Host holds machine characteristics that do not change between samples
// and are sent at the top level of the request (see plan item 4.1).
type Host struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	BootID   string `json:"boot_id"`
}

// CPU holds the shares of processor time over the interval between samples, in percent.
// The sum is not normalized: across cores it does not always add up to exactly 100.
type CPU struct {
	Cores  int     `json:"cores"`
	User   float64 `json:"user"`
	System float64 `json:"system"`
	IOWait float64 `json:"iowait"`
	Steal  float64 `json:"steal"`
	Idle   float64 `json:"idle"`
}

// Mem holds memory in bytes.
type Mem struct {
	Total     uint64 `json:"total"`
	Free      uint64 `json:"free"`
	Available uint64 `json:"available"`
	Buffers   uint64 `json:"buffers"`
	Cached    uint64 `json:"cached"`
	Shmem     uint64 `json:"shmem"`
}

// Swap holds volumes in bytes and swap rates in pages per second.
type Swap struct {
	Total   uint64  `json:"total"`
	Used    uint64  `json:"used"`
	InRate  float64 `json:"in_rate"`
	OutRate float64 `json:"out_rate"`
}

// Filesystem is a single mount point.
type Filesystem struct {
	Mount       string `json:"mount"`
	FSType      string `json:"fstype"`
	Total       uint64 `json:"total"`
	Used        uint64 `json:"used"`
	Avail       uint64 `json:"avail"`
	InodesTotal uint64 `json:"inodes_total"`
	InodesUsed  uint64 `json:"inodes_used"`
}

// Sample is a single measurement.
type Sample struct {
	TS     int64        `json:"ts"`
	Uptime uint64       `json:"uptime"`
	CPU    CPU          `json:"cpu"`
	LA     [3]float64   `json:"la"`
	Mem    Mem          `json:"mem"`
	Swap   Swap         `json:"swap"`
	FS     []Filesystem `json:"fs"`
}
