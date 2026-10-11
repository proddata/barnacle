package sqlhttp

import (
	"net/http"

	"github.com/klauspost/compress/gzhttp"
)

const gzipThreshold = 1024

var gzipWrapper = func() func(http.Handler) http.HandlerFunc {
	wrap, err := gzhttp.NewWrapper(gzhttp.MinSize(gzipThreshold))
	if err != nil {
		panic(err)
	}
	return func(h http.Handler) http.HandlerFunc { return wrap(h).ServeHTTP }
}()

func Gzip(next http.HandlerFunc) http.HandlerFunc { return gzipWrapper(next) }
