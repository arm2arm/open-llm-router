package requesttrace

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Trace previews preserve unknown JSON numbers exactly, just like forwarding.
func decodeJSON(data []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(target); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}
