package main

import (
	"strconv"
	"strings"
)

// cpuMilli parses a Kubernetes CPU quantity ("4", "250m", "123456789n", "1500u") into millicores.
func cpuMilli(q string) int64 {
	switch {
	case q == "":
		return 0
	case strings.HasSuffix(q, "n"):
		n, _ := strconv.ParseInt(strings.TrimSuffix(q, "n"), 10, 64)
		return n / 1_000_000
	case strings.HasSuffix(q, "u"):
		n, _ := strconv.ParseInt(strings.TrimSuffix(q, "u"), 10, 64)
		return n / 1_000
	case strings.HasSuffix(q, "m"):
		n, _ := strconv.ParseInt(strings.TrimSuffix(q, "m"), 10, 64)
		return n
	}
	f, _ := strconv.ParseFloat(q, 64)
	return int64(f * 1000)
}

// bytesOf parses a Kubernetes memory quantity ("16000000Ki", "512Mi", "1G", "1234") into bytes.
func bytesOf(q string) int64 {
	units := []struct {
		suffix string
		mult   int64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40},
		{"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	for _, u := range units {
		if strings.HasSuffix(q, u.suffix) {
			n, _ := strconv.ParseInt(strings.TrimSuffix(q, u.suffix), 10, 64)
			return n * u.mult
		}
	}
	n, _ := strconv.ParseInt(q, 10, 64)
	return n
}
