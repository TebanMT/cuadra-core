package sync

import "time"

func payloadTimestamp(value any) time.Time {
	switch value := value.(type) {
	case float64:
		return time.UnixMilli(int64(value)).UTC()
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
