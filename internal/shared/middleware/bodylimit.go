package middleware

import "net/http"

// MaxBody caps how much of a request body the server will read.
//
// Without it the only bounded requests are the two upload forms, which set their
// own http.MaxBytesReader — every other POST can stream for as long as the client
// keeps sending. Applied at the root so it covers the Midtrans webhook too.
//
// It is deliberately larger than any legitimate upload (2 MB service images,
// 1 MB avatars): the per-store caps are enforced inside upload.ImageStore.Save,
// which is what produces the friendly 422, and this exists only to stop a request
// nobody meant to send. MaxBytesReader also refuses on Content-Length alone, so
// an oversized body is rejected before a byte of it is read.
func MaxBody(n int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Body != nil {
				r.Body = http.MaxBytesReader(w, r.Body, n)
			}
			next.ServeHTTP(w, r)
		})
	}
}
