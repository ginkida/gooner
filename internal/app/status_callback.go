package app

import (
	"fmt"
	"time"

	"gokin/internal/ui"
)

// appStatusCallback implements client.StatusCallback to send status updates to the UI.
type appStatusCallback struct {
	app *App
}

// OnRetry is called when the client is retrying a failed request.
func (c *appStatusCallback) OnRetry(attempt, maxAttempts int, delay time.Duration, reason string) {
	if c.app == nil || c.app.program == nil {
		return
	}

	msg := fmt.Sprintf("Повторная попытка %d/%d через %s (%s)",
		attempt, maxAttempts, delay.Round(time.Second), reason)

	c.app.program.Send(ui.StatusUpdateMsg{
		Type:    ui.StatusRetry,
		Message: msg,
		Details: map[string]any{
			"attempt":     attempt,
			"maxAttempts": maxAttempts,
			"delay":       delay,
			"reason":      reason,
		},
	})
}

// OnRateLimit is called when the client is waiting due to rate limiting.
func (c *appStatusCallback) OnRateLimit(waitTime time.Duration) {
	if c.app == nil || c.app.program == nil {
		return
	}

	msg := fmt.Sprintf("Rate limit, ожидание %s...", waitTime.Round(time.Second))

	c.app.program.Send(ui.StatusUpdateMsg{
		Type:    ui.StatusRateLimit,
		Message: msg,
		Details: map[string]any{
			"waitTime": waitTime,
		},
	})
}

// OnStreamIdle is called when the streaming response has been idle for a while.
func (c *appStatusCallback) OnStreamIdle(elapsed time.Duration) {
	if c.app == nil || c.app.program == nil {
		return
	}

	msg := fmt.Sprintf("Ожидание ответа %s...", elapsed.Round(time.Second))
	if elapsed >= 20*time.Second {
		msg = fmt.Sprintf("Ожидание ответа %s... (ESC для отмены)", elapsed.Round(time.Second))
	}

	c.app.program.Send(ui.StatusUpdateMsg{
		Type:    ui.StatusStreamIdle,
		Message: msg,
		Details: map[string]any{
			"elapsed": elapsed,
		},
	})
}

// OnStreamResume is called when the stream resumes after being idle.
func (c *appStatusCallback) OnStreamResume() {
	if c.app == nil || c.app.program == nil {
		return
	}

	// Send resume message - this can be used to clear any warning toasts
	c.app.program.Send(ui.StatusUpdateMsg{
		Type:    ui.StatusStreamResume,
		Message: "",
	})
}

// OnError is called when an error occurs.
func (c *appStatusCallback) OnError(err error, recoverable bool) {
	if c.app == nil || c.app.program == nil {
		return
	}

	msg := err.Error()
	if recoverable {
		msg = "Восстанавливаемая ошибка: " + msg
	}

	c.app.program.Send(ui.StatusUpdateMsg{
		Type:    ui.StatusRecoverableError,
		Message: msg,
		Details: map[string]any{
			"recoverable": recoverable,
			"error":       err.Error(),
		},
	})
}
