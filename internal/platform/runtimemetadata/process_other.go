//go:build !darwin

package runtimemetadata

import "errors"

func InspectProcess(pid int) (Process, error) {
	return Process{}, errors.New("runtime metadata observation requires macOS")
}
