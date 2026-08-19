package upstream

import "time"

// timeValue is an optional timestamp, since IMAP servers may omit INTERNALDATE.
type timeValue struct {
	Time  time.Time
	Valid bool
}

// Or returns the timestamp if present, otherwise fallback.
func (t timeValue) Or(fallback time.Time) time.Time {
	if t.Valid {
		return t.Time
	}
	return fallback
}
