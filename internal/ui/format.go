package ui

import (
	"fmt"
	"time"
)

// byteSize writes a count of bytes: 0 B, 812 B, 4.2 KB, 18 MB, 1.25 GB.
func byteSize(n uint64) string {
	const unit = 1000
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, suffix := float64(n), ""
	for _, s := range []string{"KB", "MB", "GB", "TB"} {
		v /= unit
		suffix = s
		if v < unit {
			break
		}
	}
	switch {
	case v < 10:
		return fmt.Sprintf("%.2g %s", v, suffix)
	case v < 100:
		return fmt.Sprintf("%.1f %s", v, suffix)
	}
	return fmt.Sprintf("%.0f %s", v, suffix)
}

// rate writes bytes a second: 0 B/s, 1.2 MB/s.
func rate(n uint64) string { return byteSize(n) + "/s" }

// clock writes a time of today as 14:03:22, of other days with the date.
func clock(t, now time.Time) string {
	if t.IsZero() {
		return ""
	}
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04:05")
	}
	return t.Format("01-02 15:04")
}

// ago writes how long ago t was, coarsely.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < 5*time.Second:
		return tr("just now", "刚刚")
	case d < time.Minute:
		return trf("%d s ago", "%d 秒前", int(d.Seconds()))
	case d < time.Hour:
		return trf("%d min ago", "%d 分钟前", int(d.Minutes()))
	case d < 24*time.Hour:
		return trf("%d h ago", "%d 小时前", int(d.Hours()))
	}
	return t.Format("2006-01-02")
}

// duration writes a running time: 42 s, 5 min, 3 h 20 min.
func duration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return trf("%d s", "%d 秒", int(d.Seconds()))
	case d < time.Hour:
		return trf("%d min", "%d 分钟", int(d.Minutes()))
	}
	h, m := int(d.Hours()), int(d.Minutes())%60
	if m == 0 {
		return trf("%d h", "%d 小时", h)
	}
	return trf("%d h %d min", "%d 小时 %d 分钟", h, m)
}

// millis writes a latency: 23 ms.
func millis(d time.Duration) string { return fmt.Sprintf("%d ms", d.Milliseconds()) }
