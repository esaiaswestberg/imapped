//go:build integration

package web_test

import (
	"net/http"
	"net/http/cookiejar"
)

func newJar() http.CookieJar {
	jar, err := cookiejar.New(nil)
	if err != nil {
		panic(err)
	}
	return jar
}
