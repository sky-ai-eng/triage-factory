package tracker

import "encoding/json"

func mustJSON(v any) string {
	data, _ := json.Marshal(v)
	return string(data)
}
