package ingest

import (
	"github.com/openbankit/horizon/db2/core"
	"github.com/openbankit/horizon/db2/history"
	"github.com/openbankit/horizon/errors"
	"github.com/openbankit/horizon/ingest/session"
	"github.com/openbankit/horizon/log"
)

// Close causes the ingester to shut down.
func (i *System) Close() {
	log.Info("canceling ingestion poller")
	i.tick.Stop()
}

// ReingestAll re-ingests all ledgers
func (i *System) ReingestAll() (int, error) {
	err := i.updateLedgerState()
	if err != nil {
		return 0, err
	}
	return i.ReingestRange(1, i.coreSequence)
}

// ReingestOutdated finds old ledgers and reimports them.
func (i *System) ReingestOutdated() (n int, err error) {
	q := history.Q{Repo: i.HorizonDB}

	// NOTE: this loop will never terminate if some bug were cause a ledger
	// reingestion to silently fail.
	for {
		outdated := []int32{}
		err = q.OldestOutdatedLedgers(&outdated, CurrentVersion)
		if err != nil {
			return
		}

		if len(outdated) == 0 {
			return
		}

		log.
			WithField("lowest_sequence", outdated[0]).
			WithField("batch_size", len(outdated)).
			Info("reingest: outdated")

		var start, end int32
		flush := func() error {
			ingested, ferr := i.ReingestRange(start, end)

			if ferr != nil {
				return ferr
			}
			n += ingested
			return nil
		}

		for idx := range outdated {
			seq := outdated[idx]

			if start == 0 {
				start = seq
				end = seq
				continue
			}

			if seq == end+1 {
				end = seq
				continue
			}

			err = flush()
			if err != nil {
				return
			}

			start = seq
			end = seq
		}

		err = flush()
		if err != nil {
			return
		}
	}
}

// ReingestRange reingests a range of ledgers, from `start` to `end`, inclusive.
func (i *System) ReingestRange(start, end int32) (int, error) {
	is := session.NewSession(
		i.historySequence+1,
		i.coreSequence,
		i.HorizonDB,
		i.CoreDB,
		i.HistoryAccountCache,
		i.Metrics,
		CurrentVersion,
	)
	is.ClearExisting = true
	err := is.Run()
	return is.Ingested, err
}

// ReingestSingle re-ingests a single ledger
func (i *System) ReingestSingle(sequence int32) error {
	_, err := i.ReingestRange(sequence, sequence)
	return err
}

// Start causes the ingester to begin polling the stellar-core database for now
// ledgers and ingesting data into the horizon database.
func (i *System) Start() {
	go i.run()
}

func (i *System) run() {
	for _ = range i.tick.C {
		log.Debug("ticking ingester")
		i.runOnce()
	}
}

// run causes the importer to check stellar-core to see if we can import new
// data.
func (i *System) runOnce() {

	defer func() {
		if rec := recover(); rec != nil {
			err := errors.FromPanic(rec)
			log.WithStack(err).Errorf("import session panicked: %s", err)
			errors.ReportToSentry(err, nil)
		}
	}()

	// 1. find the latest ledger
	// 2. if any available, import until none available
	// 3. if any were imported, go to 1
	for {
		// 1.
		err := i.updateLedgerState()

		if err != nil {
			log.Errorf("could not load ledger state: %s", err)
			return
		}

		// 2.
		if i.historySequence >= i.coreSequence {
			return
		}
		is := session.NewSession(
			i.historySequence+1,
			i.coreSequence,
			i.HorizonDB,
			i.CoreDB,
			i.HistoryAccountCache,
			i.Metrics,
			CurrentVersion,
		)

		err = is.Run()

		if err != nil {
			log.WithStack(err).Errorf("import session failed: %s", err)
			return
		}

		// 3.
		if is.Ingested == 0 {
			return
		}
	}

}

func (i *System) updateLedgerState() error {
	cq := &core.Q{Repo: i.CoreDB}
	hq := &history.Q{Repo: i.HorizonDB}

	err := cq.LatestLedger(&i.coreSequence)
	if err != nil {
		return err
	}

	err = hq.LatestLedger(&i.historySequence)
	if err != nil {
		return err
	}

	return nil
}
