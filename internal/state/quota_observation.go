package state

// QuotaObservation is an actual probe result. Unlike the account's display
// snapshot, its class is not preserved after an unsuccessful request.
type QuotaObservation struct {
	Class   string
	Source  string
	Ts      int64
	UsedPct float64
}

func (d *DB) LatestQuota(tool, id string) (QuotaObservation, error) {
	var p QuotaObservation
	err := d.sql.QueryRow(`SELECT ts, IFNULL(class,''), IFNULL(source,''), IFNULL(used_pct,0) FROM quota_log WHERE tool=? AND stable_id=? ORDER BY ts DESC,rowid DESC LIMIT 1`, tool, id).
		Scan(&p.Ts, &p.Class, &p.Source, &p.UsedPct)
	return p, err
}

// LatestConfirmedQuota identifies when retained usage was last confirmed;
// a failed request must not make that record appear newer.
func (d *DB) LatestConfirmedQuota(tool, id string) (QuotaObservation, error) {
	var p QuotaObservation
	err := d.sql.QueryRow(`SELECT ts, IFNULL(class,''), IFNULL(source,''), IFNULL(used_pct,0) FROM quota_log WHERE tool=? AND stable_id=? AND class IN ('ok','soft','exhausted') ORDER BY ts DESC,rowid DESC LIMIT 1`, tool, id).
		Scan(&p.Ts, &p.Class, &p.Source, &p.UsedPct)
	return p, err
}
