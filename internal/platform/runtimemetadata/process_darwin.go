package runtimemetadata

import "golang.org/x/sys/unix"

func InspectProcess(pid int) (Process, error) {
	if pid <= 0 {
		return Process{}, ErrNotRunning
	}
	boot, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return Process{}, err
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return Process{}, err
	}
	const zombieState = 5
	if len(processes) == 0 || processes[0].Proc.P_stat == zombieState {
		return Process{}, ErrNotRunning
	}
	p := processes[0]
	return Process{PID: int(p.Proc.P_pid), UID: p.Eproc.Ucred.Uid, BootSessionID: boot, StartedAt: StartTime{p.Proc.P_starttime.Sec, int64(p.Proc.P_starttime.Usec)}}, nil
}
