//go:build darwin

package tempown

import (
	"syscall"
	"time"
)

func statusChangeTime(st *syscall.Stat_t) time.Time {
	return time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}
