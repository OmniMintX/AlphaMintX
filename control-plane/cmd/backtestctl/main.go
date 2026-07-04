// Command backtestctl drives the Phase 2 backtest engine
// (docs/specs/backtest-engine.md). `fetch` materializes a canonical dataset
// file from the Binance klines REST endpoint (cached append-only in
// backtest.db, sha256 printed for the run identity); `replay` runs a
// recorded proposals.jsonl through the Risk Gate + paper OMS over that
// dataset, teeing byte-deterministic records to records.jsonl and
// backtest_records.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/OmniMintX/AlphaMintX/control-plane/internal/backtest"
	"github.com/OmniMintX/AlphaMintX/control-plane/internal/marketdata"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "backtestctl: usage: backtestctl fetch|replay [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = runFetch(os.Args[2:])
	case "replay":
		err = runReplay(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q (want fetch or replay)", os.Args[1])
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "backtestctl: %v\n", err)
		os.Exit(1)
	}
}

// runFetch pages the venue klines endpoint over [start, end], caches the
// candles append-only, and writes the canonical dataset file. The REST base
// URL honors CONTROLPLANE_BINANCE_REST_URL (market-data.md §Endpoint
// overrides); read-only market data only.
func runFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	symbol := fs.String("symbol", "", "canonical BASE/QUOTE symbol, e.g. BTC/USDT (required)")
	interval := fs.String("interval", "", "kline interval, e.g. 1m, 1h, 1d (required)")
	start := fs.Int64("start", 0, "window start open_time, ms epoch inclusive (required)")
	end := fs.Int64("end", 0, "window end open_time, ms epoch inclusive (required)")
	market := fs.String("market", string(marketdata.MarketSpot), "binance venue: spot or futures")
	dbPath := fs.String("db", "out/backtest.db", "path to the backtest SQLite DB (klines cache)")
	outPath := fs.String("out", "out/dataset.jsonl", "path for the canonical dataset file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *symbol == "" || *interval == "" || *start == 0 || *end == 0 {
		return fmt.Errorf("fetch: -symbol, -interval, -start and -end are required")
	}
	if _, err := backtest.IntervalSeconds(*interval); err != nil {
		return err
	}

	cfg := marketdata.BinanceConfig{
		Market:  marketdata.Market(*market),
		RESTURL: os.Getenv("CONTROLPLANE_BINANCE_REST_URL"),
	}
	raw, err := marketdata.FetchKlines(context.Background(), cfg, *symbol, *interval, *start, *end)
	if err != nil {
		return err
	}
	klines := make([]backtest.Kline, 0, len(raw))
	for _, k := range raw {
		klines = append(klines, backtest.Kline{
			Symbol: *symbol, Interval: *interval, OpenTime: k.OpenTime,
			Open: k.Open, High: k.High, Low: k.Low, Close: k.Close, Volume: k.Volume,
		})
	}

	db, err := backtest.OpenDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.InsertKlines(klines); err != nil {
		return err
	}

	out, err := os.Create(*outPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	sha, err := backtest.WriteDataset(w, klines)
	if err != nil {
		out.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "backtestctl: fetched %d klines %s %s -> %s\n", len(klines), *symbol, *interval, *outPath)
	fmt.Println(sha)
	return nil
}

// runReplay runs the stage-2 replay and prints the summary.
func runReplay(args []string) error {
	fs := flag.NewFlagSet("replay", flag.ExitOnError)
	runspecPath := fs.String("runspec", "", "path to the backtest runspec (required)")
	datasetPath := fs.String("dataset", "out/dataset.jsonl", "path to the canonical dataset file")
	proposalsPath := fs.String("proposals", "out/backtest-proposals.jsonl", "path to the emitted proposal lines")
	dbPath := fs.String("db", "out/backtest.db", "path to the backtest SQLite DB")
	outPath := fs.String("out", "out/backtest-records.jsonl", "path for the records output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *runspecPath == "" {
		return fmt.Errorf("replay: -runspec is required")
	}

	spec, err := backtest.LoadRunSpec(*runspecPath)
	if err != nil {
		return err
	}
	ds, err := backtest.ReadDataset(*datasetPath)
	if err != nil {
		return err
	}
	in, err := os.Open(*proposalsPath)
	if err != nil {
		return err
	}
	defer in.Close()
	db, err := backtest.OpenDB(*dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	out, err := os.Create(*outPath)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)

	sum, runErr := backtest.Run(spec, ds, in, w, db)
	if err := w.Flush(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "backtestctl: backtest_id=%s dataset_sha256=%s ticks=%d records=%d status=%s\n",
		sum.BacktestID, sum.DatasetSHA256, sum.Ticks, sum.Records, sum.Status)
	return runErr
}
