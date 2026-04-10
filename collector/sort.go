package collector

import "sort"

// sortFloat64s wraps sort.Float64s so the only place that depends on the
// stdlib `sort` package is here. The percentile() helper in db_sql.go
// would otherwise need its own import; centralising it makes the math
// helpers easy to swap if we ever want a faster sketch (HDR histogram,
// t-digest, …).
func sortFloat64s(s []float64) {
	sort.Float64s(s)
}
