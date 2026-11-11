package store

import "time"

// SetTimingsForTest shortens the reader's wait for its one connection and the Go deadline of a whole
// call, which are 30 s and 50 s in production. Call it before the reader is used.
func (r *Reader) SetTimingsForTest(wait, deadline time.Duration) { r.wait, r.deadline = wait, deadline }
