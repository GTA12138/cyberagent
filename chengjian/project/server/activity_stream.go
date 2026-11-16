package server

import (
	"errors"
	"io"
	"net/http"
	"time"
)

var errActivityResync = errors.New("activity subscription overflow; reconnect for replay")

// Bound a blocked socket so an overflow/cancelled subscription can terminate even
// while the client stops reading. HTTP/1 and HTTP/2 both support this controller.
func writeActivityFrame(w http.ResponseWriter, frame string) error {
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(15 * time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return err
	}
	if _, err := io.WriteString(w, frame); err != nil {
		return err
	}
	return controller.Flush()
}
