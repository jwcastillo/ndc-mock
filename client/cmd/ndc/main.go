// Command ndc drives an NDC endpoint from the terminal: search, price, order
// and service a trip. It prints a readable summary by default and raw JSON with
// -json, so it works both by hand and inside a script or a skill.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"ndcmock/client/ndc"
)

func main() {
	base := flag.String("url", envOr("NDC_URL", "http://localhost:8090"), "NDC endpoint")
	version := flag.String("version", envOr("NDC_VERSION", "v192"), "NDC version segment")
	asJSON := flag.Bool("json", false, "print raw JSON instead of a summary")
	timeout := flag.Duration("timeout", 60*time.Second, "request timeout")
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}

	c := ndc.New(*base, *version)
	c.HTTP.Timeout = *timeout
	// Credentials come from the environment so they never appear in a command
	// line, where the shell history and the process table would both keep them.
	if v := os.Getenv("NDC_AUTH"); v != "" {
		c.Headers = map[string]string{"Authorization": v}
	}
	if v := os.Getenv("NDC_API_KEY"); v != "" {
		if c.Headers == nil {
			c.Headers = map[string]string{}
		}
		c.Headers["X-Api-Key"] = v
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	if err := run(ctx, c, args, *asJSON); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, c *ndc.Client, args []string, asJSON bool) error {
	switch args[0] {
	case "search":
		return cmdSearch(ctx, c, args[1:], asJSON)
	case "price":
		return one(ctx, args[1:], asJSON, "price <offer-id>", func(id string) (any, error) {
			return c.Price(ctx, id)
		})
	case "order":
		return one(ctx, args[1:], asJSON, "order <offer-id>", func(id string) (any, error) {
			return c.CreateOrder(ctx, id)
		})
	case "retrieve":
		return one(ctx, args[1:], asJSON, "retrieve <order-id>", func(id string) (any, error) {
			return c.RetrieveOrder(ctx, id)
		})
	case "cancel":
		return one(ctx, args[1:], asJSON, "cancel <order-id>", func(id string) (any, error) {
			return c.CancelOrder(ctx, id)
		})
	case "services":
		return one(ctx, args[1:], asJSON, "services <offer-id>", func(id string) (any, error) {
			return c.Services(ctx, id, 0)
		})
	case "seats":
		return one(ctx, args[1:], asJSON, "seats <offer-id>", func(id string) (any, error) {
			return c.Seats(ctx, id, 0)
		})
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func one(ctx context.Context, args []string, asJSON bool, use string, fn func(string) (any, error)) error {
	// Accept -json after the verb, and tolerate it in any position among the args.
	var rest []string
	for _, a := range args {
		if a == "-json" || a == "--json" {
			asJSON = true
			continue
		}
		rest = append(rest, a)
	}
	if len(rest) != 1 {
		return fmt.Errorf("usage: ndc %s", use)
	}
	v, err := fn(rest[0])
	if err != nil {
		return err
	}
	return emit(v, asJSON)
}

func cmdSearch(ctx context.Context, c *ndc.Client, args []string, asJSON bool) error {
	fs := flag.NewFlagSet("search", flag.ContinueOnError)
	var p ndc.SearchParams
	fs.StringVar(&p.Origin, "from", "", "origin airport code")
	fs.StringVar(&p.Dest, "to", "", "destination airport code")
	fs.StringVar(&p.DepartDate, "depart", "", "departure date, YYYY-MM-DD")
	fs.StringVar(&p.ReturnDate, "return", "", "return date for a round trip")
	fs.IntVar(&p.ADT, "adults", 1, "adult passengers")
	fs.IntVar(&p.CHD, "children", 0, "child passengers")
	fs.IntVar(&p.INF, "infants", 0, "infant passengers")
	fs.StringVar(&p.Airline, "airline", "", "carrier code to present offers as")
	fs.IntVar(&p.MaxOffers, "max", ndc.DefaultMaxOffers, "offers to show")
	// -json is accepted here as well as before the subcommand: putting a flag
	// after the verb is the natural way to type it.
	local := fs.Bool("json", false, "print raw JSON instead of a summary")
	if err := fs.Parse(args); err != nil {
		return err
	}
	asJSON = asJSON || *local
	res, err := c.Search(ctx, p)
	if err != nil {
		return err
	}
	if asJSON {
		return emit(res, true)
	}
	printShopping(res)
	return nil
}

func printShopping(s *ndc.Shopping) {
	route := s.Origin + " -> " + s.Destination + "  " + s.DepartureDate
	if s.ReturnDate != "" {
		route += " / " + s.ReturnDate
	}
	fmt.Printf("%s\n%d offers", route, s.OfferCount)
	if s.Truncated {
		fmt.Printf(" (showing %d)", len(s.Offers))
	}
	fmt.Printf("\n\n")

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "PRICE\tROUTING\tOFFER ID")
	for _, o := range s.Offers {
		var legs []string
		for _, sg := range o.Segments {
			legs = append(legs, fmt.Sprintf("%s-%s %s%s", sg.Origin, sg.Destination, sg.Carrier, sg.FlightNum))
		}
		fmt.Fprintf(w, "%s %d\t%s\t%s\n",
			o.Price.Currency, o.Price.Total, strings.Join(legs, " / "), truncate(o.OfferID, 32))
	}
	_ = w.Flush()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func emit(v any, asJSON bool) error {
	if !asJSON {
		if o, ok := v.(*ndc.Order); ok {
			fmt.Printf("order %s  %s  %s %d\n", o.OrderID, o.Status, o.Currency, o.Total)
			if len(o.History) > 0 {
				fmt.Printf("history: %s\n", strings.Join(o.History, " -> "))
			}
			return nil
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprint(os.Stderr, `ndc - drive an NDC endpoint from the terminal

usage: ndc [flags] <command>

commands:
  search -from LIM -to SCL -depart 2026-09-30 [-return ...] [-airline IB] [-max 10]
  price <offer-id>
  order <offer-id>
  retrieve <order-id>
  cancel <order-id>
  services <offer-id>
  seats <offer-id>

flags:
  -url        NDC endpoint (default http://localhost:8090, or $NDC_URL)
  -version    NDC version segment (default v192, or $NDC_VERSION)
  -json       print raw JSON instead of a summary
  -timeout    request timeout

credentials, when the endpoint needs them, come from the environment:
  NDC_AUTH      sent as Authorization
  NDC_API_KEY   sent as X-Api-Key
`)
}
