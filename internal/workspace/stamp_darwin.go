package workspace

import (
	"fmt"
	"golang.org/x/sys/unix"
	"time"
)

func nativeStamp(s *unix.Stat_t) (string, time.Time, time.Time) {
	mt, ct := time.Unix(s.Mtim.Sec, s.Mtim.Nsec), time.Unix(s.Ctim.Sec, s.Ctim.Nsec)
	return fmt.Sprintf("%d:%d:%d:%d:%d:%d", s.Dev, s.Ino, s.Size, s.Mode, mt.UnixNano(), ct.UnixNano()), mt, ct
}
