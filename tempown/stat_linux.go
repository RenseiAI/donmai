//go:build linux

package tempown

import (
	"syscall"
	"time"
)

func statusChangeTime(st *syscall.Stat_t) time.Time {
	return time.Unix(st.Ctim.Sec, st.Ctim.Nsec)
}
