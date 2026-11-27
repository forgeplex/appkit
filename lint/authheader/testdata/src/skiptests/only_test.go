package skiptests

import "net/http"

func f(r *http.Request) {
	_ = r.Header.Get("Authorization")
}
