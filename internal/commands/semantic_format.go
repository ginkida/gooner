package commands

import (
	"fmt"
	"time"
)

// formatBytes formats a byte size in human-readable format.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}

// formatTime formats a unix timestamp in a human-readable way.
func formatTime(unix int64) string {
	if unix == 0 {
		return "never"
	}

	t := time.Unix(unix, 0)
	age := time.Since(t)

	if age < time.Minute {
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	} else if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	} else if age < 24*time.Hour {
		hours := int(age.Hours())
		minutes := int(age.Minutes()) % 60
		return fmt.Sprintf("%dh %dm ago", hours, minutes)
	} else if age < 30*24*time.Hour {
		days := int(age.Hours() / 24)
		hours := int(age.Hours()) % 24
		return fmt.Sprintf("%dd %dh ago", days, hours)
	} else {
		return t.Format("2006-01-02")
	}
}

// formatDuration formats a duration in a human-readable way.
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	} else if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	} else if d < time.Hour {
		minutes := int(d.Minutes())
		seconds := int(d.Seconds()) % 60
		return fmt.Sprintf("%dm %ds", minutes, seconds)
	} else {
		hours := int(d.Hours())
		minutes := int(d.Minutes()) % 60
		return fmt.Sprintf("%dh %dm", hours, minutes)
	}
}
