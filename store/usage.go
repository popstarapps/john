package store

import (
	"time"
)

// UsageDelta is what one event adds to the daily rollups.
type UsageDelta struct {
	Requests     int
	TokensIn     int
	TokensOut    int
	TokensCached int
	Sessions     int
	Allowed      int
	Approvals    int
	Blocked      int
	CostUSD      float64
}

// AddUsage folds a delta into the (day, user, model) rollup row.
func (s *Store) AddUsage(day time.Time, userID, modelID string, d UsageDelta) error {
	_, err := s.db.Exec(`INSERT INTO usage_days
		(date, user_id, model_id, requests, tokens_in, tokens_out, tokens_cached, sessions, allowed, approvals, blocked, cost_usd)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (date, user_id, model_id) DO UPDATE SET
			requests = requests + excluded.requests,
			tokens_in = tokens_in + excluded.tokens_in,
			tokens_out = tokens_out + excluded.tokens_out,
			tokens_cached = tokens_cached + excluded.tokens_cached,
			sessions = sessions + excluded.sessions,
			allowed = allowed + excluded.allowed,
			approvals = approvals + excluded.approvals,
			blocked = blocked + excluded.blocked,
			cost_usd = cost_usd + excluded.cost_usd`,
		day.UTC().Format("2006-01-02"), userID, modelID,
		d.Requests, d.TokensIn, d.TokensOut, d.TokensCached, d.Sessions,
		d.Allowed, d.Approvals, d.Blocked, d.CostUSD)
	return err
}

// UsageTotals is a window of usage summed up.
type UsageTotals struct {
	Requests     int
	TokensIn     int
	TokensOut    int
	TokensCached int
	Sessions     int
	Allowed      int
	Approvals    int
	Blocked      int
	CostUSD      float64
}

// UsageDay is one day of the window, split by model.
type UsageDay struct {
	Date    string // YYYY-MM-DD
	Totals  UsageTotals
	ByModel []UsageByModel
}

// UsageByModel is an aggregate attributed to one model.
type UsageByModel struct {
	ModelID  string
	Requests int
	Tokens   int
	CostUSD  float64
}

// UsageByActor is an aggregate attributed to one user or team.
type UsageByActor struct {
	ID       string
	Requests int
	Sessions int
	CostUSD  float64
}

// UsageSummary answers the console's usage page: the window's days (zero
// days included, so charts have a continuous axis), totals, the same-length
// window before it, and splits by model, team, and user. userID empty means
// the whole organization.
func (s *Store) UsageSummary(userID string, days int, now time.Time) (dayRows []UsageDay, totals, previous UsageTotals, byModel []UsageByModel, byTeam, byUser []UsageByActor, err error) {
	now = now.UTC()
	start := now.AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	prevStart := now.AddDate(0, 0, -(2*days - 1)).Format("2006-01-02")

	filter := ""
	args := []any{start}
	if userID != "" {
		filter = ` AND user_id = ?`
		args = append(args, userID)
	}

	// Per-day, per-model rows for the current window.
	rows, err := s.db.Query(`SELECT date, model_id,
			SUM(requests), SUM(tokens_in), SUM(tokens_out), SUM(tokens_cached),
			SUM(sessions), SUM(allowed), SUM(approvals), SUM(blocked), SUM(cost_usd)
		FROM usage_days WHERE date >= ?`+filter+`
		GROUP BY date, model_id ORDER BY date, model_id`, args...)
	if err != nil {
		return
	}
	defer rows.Close()

	byDate := map[string]*UsageDay{}
	modelOrder := []string{}
	modelSeen := map[string]*UsageByModel{}
	for rows.Next() {
		var date, modelID string
		var t UsageTotals
		if err = rows.Scan(&date, &modelID, &t.Requests, &t.TokensIn, &t.TokensOut, &t.TokensCached,
			&t.Sessions, &t.Allowed, &t.Approvals, &t.Blocked, &t.CostUSD); err != nil {
			return
		}
		d := byDate[date]
		if d == nil {
			d = &UsageDay{Date: date, ByModel: []UsageByModel{}}
			byDate[date] = d
		}
		d.Totals.add(t)
		tokens := t.TokensIn + t.TokensOut
		if modelID != "" {
			d.ByModel = append(d.ByModel, UsageByModel{ModelID: modelID, Requests: t.Requests, Tokens: tokens, CostUSD: t.CostUSD})
			agg := modelSeen[modelID]
			if agg == nil {
				agg = &UsageByModel{ModelID: modelID}
				modelSeen[modelID] = agg
				modelOrder = append(modelOrder, modelID)
			}
			agg.Requests += t.Requests
			agg.Tokens += tokens
			agg.CostUSD += t.CostUSD
		}
		totals.add(t)
	}
	if err = rows.Err(); err != nil {
		return
	}

	// A continuous axis: every day of the window, zeros where nothing happened.
	for i := 0; i < days; i++ {
		date := now.AddDate(0, 0, -(days - 1 - i)).Format("2006-01-02")
		if d := byDate[date]; d != nil {
			dayRows = append(dayRows, *d)
		} else {
			dayRows = append(dayRows, UsageDay{Date: date, ByModel: []UsageByModel{}})
		}
	}
	for _, id := range modelOrder {
		byModel = append(byModel, *modelSeen[id])
	}

	// The window immediately before, totals only.
	prevArgs := []any{prevStart, start}
	if userID != "" {
		prevArgs = append(prevArgs, userID)
	}
	err = s.db.QueryRow(`SELECT COALESCE(SUM(requests),0), COALESCE(SUM(tokens_in),0), COALESCE(SUM(tokens_out),0),
			COALESCE(SUM(tokens_cached),0), COALESCE(SUM(sessions),0), COALESCE(SUM(allowed),0),
			COALESCE(SUM(approvals),0), COALESCE(SUM(blocked),0), COALESCE(SUM(cost_usd),0)
		FROM usage_days WHERE date >= ? AND date < ?`+filter, prevArgs...).Scan(
		&previous.Requests, &previous.TokensIn, &previous.TokensOut, &previous.TokensCached,
		&previous.Sessions, &previous.Allowed, &previous.Approvals, &previous.Blocked, &previous.CostUSD)
	if err != nil {
		return
	}

	// Org scope: split the window by user, and by team through membership. A
	// user on several teams is counted on each — a share of attention, not a
	// conserved quantity.
	if userID == "" {
		byUser, err = s.usageActors(`SELECT user_id, SUM(requests), SUM(sessions), SUM(cost_usd)
			FROM usage_days WHERE date >= ? GROUP BY user_id ORDER BY SUM(cost_usd) DESC`, start)
		if err != nil {
			return
		}
		byTeam, err = s.usageActors(`SELECT tm.team_id, SUM(u.requests), SUM(u.sessions), SUM(u.cost_usd)
			FROM usage_days u JOIN team_members tm ON tm.user_id = u.user_id
			WHERE u.date >= ? GROUP BY tm.team_id ORDER BY SUM(u.cost_usd) DESC`, start)
		if err != nil {
			return
		}
	}
	return
}

func (t *UsageTotals) add(o UsageTotals) {
	t.Requests += o.Requests
	t.TokensIn += o.TokensIn
	t.TokensOut += o.TokensOut
	t.TokensCached += o.TokensCached
	t.Sessions += o.Sessions
	t.Allowed += o.Allowed
	t.Approvals += o.Approvals
	t.Blocked += o.Blocked
	t.CostUSD += o.CostUSD
}

func (s *Store) usageActors(query string, args ...any) ([]UsageByActor, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsageByActor
	for rows.Next() {
		var a UsageByActor
		if err := rows.Scan(&a.ID, &a.Requests, &a.Sessions, &a.CostUSD); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
