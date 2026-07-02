package mcp

import (
	"crypto/sha1"
	"fmt"
)

func sha1sum(s string) string {
	h := sha1.Sum([]byte(s))
	return fmt.Sprintf("%x", h[:])
}
