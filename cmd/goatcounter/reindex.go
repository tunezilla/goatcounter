// Copyright © 2019 Martin Tournoij – This file is part of GoatCounter and
// published under the terms of a slightly modified EUPL v1.2 license, which can
// be found in the LICENSE file or at https://license.goatcounter.com

package main

import (
	"context"
	"fmt"
	"time"

	nnow "github.com/jinzhu/now"
	"zgo.at/gadget"
	"zgo.at/goatcounter"
	"zgo.at/goatcounter/cron"
	"zgo.at/zdb"
	"zgo.at/zli"
	"zgo.at/zlog"
	"zgo.at/zvalidate"
)

// reindex
const usageReindex = `
GoatCounter keeps several *_stats and *_count tables so it's less expensive to
generate charts. These are normally updated automatically in the background.
This command recreates these tables. This is mostly for upgrades; you shouldn't
have to run this in normal usage.

This command may take a while to run on larger sites.

For SQLite you may want to stop the main GoatCounter process first, or you're
likely to get locking errors. For PostgreSQL this shouldn't be an issue.

Flags:

  -db          Database connection: "sqlite://<file>" or "postgres://<connect>"
               See "goatcounter help db" for detailed documentation. Default:
               sqlite://db/goatcounter.sqlite3?_busy_timeout=200&_journal_mode=wal&cache=shared

  -debug       Modules to debug, comma-separated or 'all' for all modules.
               See "goatcounter help debug" for a list of modules.

  -pause       Number of seconds to pause after each month, to give the server
               some breathing room on large sites. Default: 0.

  -since       Reindex only statistics since this month instead of all of them;
               as year-month in UTC.

  -to          Reindex only statistics up to and including this month; as
               year-month in UTC. The default is the current month.

  -table       Which tables to reindex: hit_stats, hit_counts, browser_stats,
               system_stats, location_stats, ref_counts, size_stats, or all
               (default).

  -useragents  Redo the bot and browser/system detection on all User-Agent headrs.

  -site        Only reindex this site ID. Default is to reindex all.

  -silent      Don't print progress.
`

// TODO: re-do the way this works. Instead of operating on the database directly
// send a signal to goatcounter to reindex stuff. This makes it easier to deal
// with locking from the application level, especially for SQLite.
func cmdReindex(f zli.Flags, ready chan<- struct{}, stop chan struct{}) error {
	defer func() { ready <- struct{}{} }()

	var (
		dbConnect = f.String("sqlite://db/goatcounter.sqlite3", "db").Pointer()
		debug     = f.String("", "debug").Pointer()
		since     = f.String("", "since").Pointer()
		to        = f.String("", "to").Pointer()
		tables    = f.StringList([]string{"all"}, "table").Pointer()
		pause     = f.Int(0, "pause").Pointer()
		silent    = f.Bool(false, "silent").Pointer()
		doUA      = f.Bool(false, "useragents").Pointer()
		site      = f.Int64(0, "site").Pointer()
	)
	err := f.Parse()
	if err != nil {
		return err
	}

	return func(dbConnect, debug, since, to string, tables []string, pause int, silent, doUA bool, site int64) error {
		v := zvalidate.New()
		firstDay := v.Date("-since", since, "2006-01")
		lastDay := v.Date("-to", to, "2006-01")

		for _, t := range tables {
			v.Include("-table", t, []string{"hit_stats", "hit_counts",
				"browser_stats", "system_stats", "location_stats",
				"ref_counts", "size_stats", "all", ""})
		}
		if v.HasErrors() {
			return v
		}

		zlog.Config.SetDebug(debug)

		db, ctx, err := connectDB(dbConnect, nil, false, false)
		if err != nil {
			return err
		}
		defer db.Close()

		if doUA {
			err = userAgents(ctx, silent)
			if err != nil {
				return err
			}
		}

		if len(tables) == 0 || (len(tables) == 1 && tables[0] == "") {
			return nil
		}

		if since == "" {
			w := ""
			if site > 0 {
				w = fmt.Sprintf(" where site_id=%d ", site)
			}

			var first string
			err := db.Get(ctx, &first, `select created_at from hits `+w+` order by created_at asc limit 1`)
			if err != nil {
				if zdb.ErrNoRows(err) {
					return nil
				}
				return err
			}

			firstDay, err = time.Parse("2006-01", first[:7])
			if err != nil {
				return err
			}
		}
		if to == "" {
			lastDay = time.Now().UTC()
		}

		var sites goatcounter.Sites
		err = sites.UnscopedList(ctx)
		if err != nil {
			return err
		}

		firstDay = nnow.New(firstDay).BeginningOfMonth()
		lastDay = nnow.New(lastDay).EndOfMonth()

		for i, s := range sites {
			if site > 0 && s.ID != site {
				continue
			}
			err := dosite(ctx, s, tables, pause, firstDay, lastDay, silent, len(sites), i+1)
			if err != nil {
				return err
			}
		}

		if !silent {
			fmt.Fprintln(zli.Stdout, "")
		}
		return nil
	}(*dbConnect, *debug, *since, *to, *tables, *pause, *silent, *doUA, *site)
}

func dosite(
	ctx context.Context, site goatcounter.Site, tables []string,
	pause int, firstDay, lastDay time.Time, silent bool,
	nsites, isite int,
) error {
	siteID := site.ID

	if firstDay.Before(site.FirstHitAt) {
		firstDay = site.FirstHitAt
	}

	now := goatcounter.Now()
	now = time.Date(now.Year(), now.Month(), now.Day(), 23, 59, 59, 0, time.UTC)

	months := [][]time.Time{
		{firstDay, nnow.With(firstDay).EndOfMonth()},
	}

	start := nnow.With(nnow.With(firstDay).EndOfMonth().Add(12 * time.Hour)).BeginningOfMonth()
	for {
		if start.After(now) {
			break
		}

		end := nnow.With(start).EndOfMonth()
		if end.After(lastDay) {
			months = append(months, []time.Time{start, lastDay})
			break
		}

		months = append(months, []time.Time{start, end})
		start = nnow.With(end.Add(12 * time.Hour)).BeginningOfMonth()
	}

	query := `select * from hits where site_id=$1 and bot=0 and created_at>=$2 and created_at<=$3`

	var pauses time.Duration
	if pause > 0 {
		pauses = time.Duration(pause) * time.Second
	}

	for _, month := range months {
		err := zdb.TX(ctx, func(ctx context.Context) error {
			if zdb.PgSQL(ctx) {
				err := zdb.Exec(ctx, `lock table hits, hit_counts, hit_stats, size_stats, location_stats, browser_stats, system_stats
					in exclusive mode`)
				if err != nil {
					return err
				}
			}

			var hits []goatcounter.Hit
			err := zdb.Select(ctx, &hits, query, siteID, dayStart(month[0]), dayEnd(month[1]))
			if err != nil {
				return err
			}

			if !silent {
				fmt.Fprintf(zli.Stdout, "\r\x1b[0Ksite %d (%d/%d) %s → %d", siteID, isite, nsites, month[0].Format("2006-01"), len(hits))
			}

			clearMonth(ctx, tables, month[0].Format("2006-01"), siteID)

			return cron.ReindexStats(ctx, site, hits, tables)
		})
		if err != nil {
			return err
		}

		if pauses > 0 {
			time.Sleep(pauses)
		}
	}

	return nil
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func clearMonth(ctx context.Context, tables []string, month string, siteID int64) {
	where := fmt.Sprintf(" where site_id=%d and cast(day as varchar) like '%s-__'", siteID, month)
	for _, t := range tables {
		switch t {
		case "hit_stats":
			must(zdb.Exec(ctx, `delete from hit_stats`+where))
		case "hit_counts":
			must(zdb.Exec(ctx, fmt.Sprintf(
				`delete from hit_counts where site_id=%d and cast(hour as varchar) like '%s-%%'`,
				siteID, month)))
		case "browser_stats":
			must(zdb.Exec(ctx, `delete from browser_stats`+where))
		case "system_stats":
			must(zdb.Exec(ctx, `delete from system_stats`+where))
		case "location_stats":
			must(zdb.Exec(ctx, `delete from location_stats`+where))
		case "ref_counts":
			must(zdb.Exec(ctx, fmt.Sprintf(
				`delete from ref_counts where site_id=%d and cast(hour as varchar) like '%s-%%'`,
				siteID, month)))
		case "size_stats":
			must(zdb.Exec(ctx, `delete from size_stats`+where))
		case "all":
			must(zdb.Exec(ctx, `delete from hit_stats`+where))
			must(zdb.Exec(ctx, `delete from browser_stats`+where))
			must(zdb.Exec(ctx, `delete from system_stats`+where))
			must(zdb.Exec(ctx, `delete from location_stats`+where))
			must(zdb.Exec(ctx, `delete from size_stats`+where))
			must(zdb.Exec(ctx, fmt.Sprintf(
				`delete from hit_counts where site_id=%d and cast(hour as varchar) like '%s-%%'`,
				siteID, month)))
			must(zdb.Exec(ctx, fmt.Sprintf(
				`delete from ref_counts where site_id=%d and cast(hour as varchar) like '%s-%%'`,
				siteID, month)))
		}
	}
}

func dayStart(t time.Time) string { return t.Format("2006-01-02") + " 00:00:00" }
func dayEnd(t time.Time) string   { return t.Format("2006-01-02") + " 23:59:59" }

func userAgents(ctx context.Context, silent bool) error {
	var uas []goatcounter.UserAgent
	err := zdb.Select(ctx, &uas, `select * from user_agents`)
	if err != nil {
		return err
	}

	for i, ua := range uas {
		ua.UserAgent = gadget.Unshorten(ua.UserAgent)
		err := ua.Update(ctx)
		if err != nil {
			return err
		}

		if !silent {
			if i%500 == 0 {
				zli.ReplaceLinef("user_agent %d of %d", i, len(uas))
			}
		}
	}
	if !silent {
		fmt.Fprintln(zli.Stdout)
	}
	return nil
}
