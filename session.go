package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"flactastic/server/api"
)

// session shows what the running server is streaming: a live board on a
// terminal, one listing otherwise, or the log of finished streams.
func session(o opts) error {
	sub := ""
	if len(o.args) > 1 {
		sub = o.args[1]
	}
	if len(o.args) > 2 || (sub != "" && sub != "list" && sub != "log") {
		return fmt.Errorf("unknown command %q; see --help", strings.Join(o.args, " "))
	}
	client := http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(o.data, "admin.sock"))
		}}}
	live := sub == "" && isTerminal(1)
	sent, at := map[int64]int64{}, time.Now()
	for {
		width, err := terminalWidth()
		if err != nil {
			width = 140
		}
		res, err := client.Get("http://server/?" + sub)
		if errors.Is(err, os.ErrPermission) {
			return errors.New("not allowed to ask the server; run this with sudo")
		} else if err != nil {
			return errors.New("the server isn't running, or is older than this command")
		}
		var v api.Sessions
		err = json.NewDecoder(res.Body).Decode(&v)
		res.Body.Close()
		if err != nil {
			return err
		}
		var b strings.Builder
		if live {
			b.WriteString("\x1b[H\x1b[2J")
		}
		if sub == "log" {
			printLog(&b, v.Log, o.user, width)
		} else {
			printBoard(&b, v, o.user, width, live, sent, time.Since(at).Seconds())
		}
		os.Stdout.WriteString(b.String())
		if !live {
			return nil
		}
		at = time.Now()
		time.Sleep(time.Second)
	}
}

// fit pads or cuts s to n columns.
func fit(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:max(n-1, 0)]) + "…"
	}
	return fmt.Sprintf("%-*s", n, s)
}

func clock(d time.Duration) string {
	s := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func size(n float64) string {
	for _, u := range []string{"B", "KB", "MB"} {
		if n < 1000 {
			return fmt.Sprintf("%.3g %s", n, u)
		}
		n /= 1000
	}
	return fmt.Sprintf("%.3g GB", n)
}

func name(s api.Stream) string {
	if s.Artist == "" {
		return s.Title
	}
	return s.Artist + " - " + s.Title
}

// printBoard writes two lines per stream: who is playing what, then the
// connection and source details. sent holds each stream's byte count from
// the last board, for the rate.
func printBoard(b *strings.Builder, v api.Sessions, user string, width int, live bool, sent map[int64]int64, dt float64) {
	var rows []api.Stream
	sending := 0
	for _, s := range v.Live {
		if user == "" || s.User == user {
			rows = append(rows, s)
			sending += min(s.Sending, 1)
		}
	}
	fmt.Fprintf(b, "FLACtastic sessions  %s  %d sending, %d idle  transcodes %d/%d", time.Now().Format("15:04:05"),
		sending, len(rows)-sending, v.Transcodes, v.Slots)
	if live {
		b.WriteString("  Ctrl-C to quit")
	}
	if b.WriteString("\n\n"); len(rows) == 0 {
		b.WriteString("Nothing is streaming.\n")
		return
	}
	track := max(width-100, 20)
	fmt.Fprintf(b, "%4s  %s %s %s %s %9s %10s  %s\n", "ID", fit("USER", 12), fit("DEVICE", 22), fit("TRACK", track),
		fit("QUALITY", 9), "SENT", "RATE", "STATE        AGE/LENGTH")
	for _, s := range rows {
		rate, state := "-", "idle "+clock(time.Since(s.Ended))
		if before, ok := sent[s.ID]; ok && live && dt > 0 {
			rate = size(float64(s.Bytes-before)/dt) + "/s"
		}
		if sent[s.ID] = s.Bytes; s.Sending > 0 {
			state = "sending"
		}
		fmt.Fprintf(b, "%4d  %s %s %s %s %9s %10s  %s %s/%s\n", s.ID, fit(s.User, 12), fit(s.Device+" ("+s.Kind+")", 22),
			fit(name(s), track), fit(s.Quality, 9), size(float64(s.Bytes)), rate, fit(state, 12),
			clock(time.Since(s.Started)), clock(time.Duration(s.Duration*float64(time.Second))))
		detail := fmt.Sprintf("%s  %s  %s %d Hz", s.Addr, s.Proto, strings.ToUpper(s.Format), s.SampleRate)
		if s.BitDepth > 0 {
			detail += fmt.Sprintf(" %d-bit", s.BitDepth)
		}
		if s.Offset != "" {
			detail += "  " + s.Offset
		}
		detail += fmt.Sprintf("  requests %d, cancelled %d", s.Requests, s.Cancelled)
		if s.Error != "" {
			detail += "  ERROR " + s.Error
		}
		detail = strings.TrimRight(fit(detail+"  "+s.Path+"  "+s.Agent, width-6), " ")
		if live {
			detail = "\x1b[2m" + detail + "\x1b[0m"
		}
		fmt.Fprintf(b, "      %s\n", detail)
	}
}

func printLog(b *strings.Builder, log []api.Stream, user string, width int) {
	track := max(width-93, 20)
	fmt.Fprintf(b, "%s %s %s %s %s %9s %4s %4s  %s\n", fit("STARTED", 15), fit("USER", 12), fit("DEVICE", 22),
		fit("TRACK", track), fit("QUALITY", 9), "SENT", "REQ", "CXL", "ERROR")
	for _, s := range log {
		if user == "" || s.User == user {
			fmt.Fprintf(b, "%s %s %s %s %s %9s %4d %4d  %s\n", s.Started.Local().Format("Jan 02 15:04:05"), fit(s.User, 12),
				fit(s.Device+" ("+s.Kind+")", 22), fit(name(s), track), fit(s.Quality, 9), size(float64(s.Bytes)),
				s.Requests, s.Cancelled, s.Error)
		}
	}
}
