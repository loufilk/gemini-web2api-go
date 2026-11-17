package app

import (
	"encoding/json"
	"regexp"
	"strings"
)

var rcidRe = regexp.MustCompile(`"(rc_[A-Za-z0-9_-]{6,})"`)

// parseConvIDs 从 StreamGenerate 响应里取续接要用的四样：cid=[1][0]、rid=[1][1]、
// tok26=帧[26]、rcid=正文里的 rc_xxx（登录态续接第 3 位要用它，匿名为空）。
func parseConvIDs(raw string) (cid, rid, rcid, tok26 string) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[") {
			continue
		}
		var arr []interface{}
		if json.Unmarshal([]byte(line), &arr) != nil {
			continue
		}
		for _, it := range arr {
			row, ok := it.([]interface{})
			if !ok || len(row) < 3 || row[0] != "wrb.fr" {
				continue
			}
			payload, ok := row[2].(string)
			if !ok {
				continue
			}
			var f []interface{}
			if json.Unmarshal([]byte(payload), &f) != nil {
				continue
			}
			if len(f) > 1 {
				if meta, ok := f[1].([]interface{}); ok && len(meta) >= 2 {
					if s, ok := meta[0].(string); ok && s != "" {
						cid = s
					}
					if s, ok := meta[1].(string); ok && s != "" {
						rid = s
					}
				}
			}
			if len(f) > 26 {
				if s, ok := f[26].(string); ok && s != "" {
					tok26 = s
				}
			}
		}
	}
	if m := rcidRe.FindStringSubmatch(raw); m != nil {
		rcid = m[1]
	}
	return
}
