package main

import (
	"context"
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const defaultHeavyURLs = "https://en.wikipedia.org/wiki/World_War_II,https://www.bbc.com/news,https://github.com/torvalds/linux,https://www.youtube.com/,https://www.theverge.com/"

func cmdMemory(args []string) error {
	fs := flag.NewFlagSet("memory", flag.ExitOnError)
	n := fs.Int("browsers", 3, "number of browser containers")
	tabs := fs.Int("tabs", 4, "tabs per browser")
	urlsFlag := fs.String("urls", defaultHeavyURLs, "comma-separated URLs, cycled across tabs")
	mode := fs.String("mode", "headless", "headless or xvfb")
	wait := fs.Duration("wait", 60*time.Second, "settle time before sampling")
	_ = fs.Parse(args)
	urls := strings.Split(*urlsFlag, ",")
	// Serve attack/ on 127.0.0.1:7788 too, so local test pages work.
	if srv, err := startHTTP(nil); err == nil {
		defer srv.Close()
	}

	var bs []*Browser
	defer func() {
		for _, b := range bs {
			b.Remove()
		}
	}()
	k := 0
	for i := 0; i < *n; i++ {
		b, err := StartBrowser(fmt.Sprintf("mem%d", i+1), *mode, "")
		if err != nil {
			return err
		}
		bs = append(bs, b)
		for t := 0; t < *tabs; t++ {
			u := strings.TrimSpace(urls[k%len(urls)])
			k++
			err := chromedp.Run(b.Ctx, chromedp.ActionFunc(func(ctx context.Context) error {
				if t == 0 {
					// reuse the initial about:blank tab for the first URL
					return chromedp.Navigate(u).Do(ctx)
				}
				_, err := target.CreateTarget(u).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser))
				return err
			}))
			if err != nil {
				fmt.Printf("  %s tab %d (%s): %v\n", b.Container, t+1, u, err)
			}
		}
		fmt.Printf("  %s: %d tabs opened\n", b.Container, *tabs)
	}
	fmt.Printf("waiting %s for pages to settle\n", *wait)
	time.Sleep(*wait)

	names := []string{"stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}"}
	for _, b := range bs {
		names = append(names, b.Container)
	}
	statsOut, err := docker(names...)
	if err != nil {
		return err
	}
	stats := map[string][2]string{}
	for _, line := range strings.Split(strings.TrimSpace(statsOut), "\n") {
		f := strings.Split(line, "\t")
		if len(f) == 3 {
			stats[f[0]] = [2]string{f[1], f[2]}
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n### memory, %d browsers x %d tabs, mode %s, %s\n\nURLs: %s\n\n", *n, *tabs, *mode, time.Now().Format(time.RFC3339), *urlsFlag)
	sb.WriteString("| container | docker stats mem | CPU | chromium procs | sum RSS MB | sum PSS MB |\n|---|---|---|---|---|---|\n")
	var totRSS, totPSS float64
	for _, b := range bs {
		rssOut, _ := docker("exec", b.Container, "ps", "-o", "rss=", "-C", "chromium")
		var rss int64
		procs := 0
		for _, f := range strings.Fields(rssOut) {
			v, _ := strconv.ParseInt(f, 10, 64)
			rss += v
			procs++
		}
		pssOut, _ := docker("exec", "-u", "chrome", b.Container, "sh", "-c",
			`for p in $(pgrep -x chromium); do cat /proc/$p/smaps_rollup 2>/dev/null; done | awk '/^Pss:/{s+=$2} END{print s+0}'`)
		pss, _ := strconv.ParseInt(strings.TrimSpace(pssOut), 10, 64)
		st := stats[b.Container]
		fmt.Fprintf(&sb, "| %s | %s | %s | %d | %.0f | %.0f |\n", b.Container, st[0], st[1], procs, float64(rss)/1024, float64(pss)/1024)
		totRSS += float64(rss) / 1024
		totPSS += float64(pss) / 1024
	}
	fmt.Fprintf(&sb, "| **total** | | | | %.0f | %.0f |\n", totRSS, totPSS)
	sb.WriteString("\nRSS double-counts shared pages across Chromium processes; PSS splits them and is the better per-browser figure.\n")
	fmt.Print(sb.String())
	return appendFindings(sb.String())
}
