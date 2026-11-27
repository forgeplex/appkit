package skiptests

import "strings"

func f(err error) bool {
	return strings.Contains(err.Error(), "boom")
}
