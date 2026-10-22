package connect

// OnJoin sets r's test seam: fn runs as a caller joins a run in flight.
func OnJoin(r *Refresher, fn func(key string)) { r.onJoin = fn }
