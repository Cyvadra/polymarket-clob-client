// Command polywallet moves funds in a deposit wallet (signature type 3)
// through Polymarket's gasless relayer:
//
//	polywallet split      --condition 0x… --amount 10        # 10 pUSD -> 10 Up + 10 Down
//	polywallet send-token --to 0x… --token-id 6751… --amount 5
//
// Amounts are decimal pUSD or shares (6 decimals). Each command checks the
// balance, prints the calls it would make and stops there; add --yes to sign
// and submit them, after which it waits for the relayer to confirm.
//
// It is a local test tool (deploy.sh does not ship it); see scripts/README.md
// for running it against a host's executiond.env. It reads the same
// POLYMARKET_* environment as executiond, which must include
// POLYMARKET_RELAYER_API_KEY (and POLYMARKET_RELAYER_API_KEY_ADDRESS when the
// key was issued to an address other than the signer).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	clobclient "github.com/Cyvadra/polymarket-clob-client"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "polywallet: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("a subcommand is required")
	}
	cmd, args := args[0], args[1:]
	flags := flag.NewFlagSet(cmd, flag.ContinueOnError)
	var conditionID, to, tokenID, amountText string
	var yes bool
	var wait time.Duration
	flags.StringVar(&amountText, "amount", "", "amount in pUSD or shares, e.g. 12.5")
	flags.BoolVar(&yes, "yes", false, "sign and submit; without it only the plan is printed")
	flags.DurationVar(&wait, "wait", 3*time.Minute, "how long to wait for the relayer to confirm")
	switch cmd {
	case "split":
		flags.StringVar(&conditionID, "condition", "", "market condition ID (0x…, 32 bytes)")
	case "send-token":
		flags.StringVar(&to, "to", "", "recipient address")
		flags.StringVar(&tokenID, "token-id", "", "outcome token ID (decimal)")
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", cmd)
	}
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		// flag already printed the error and usage.
		os.Exit(2)
	}
	amount, err := clobclient.ParseUnits(amountText)
	if err != nil {
		return fmt.Errorf("--amount: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cfg, err := clobclient.ConfigFromEnv()
	if err != nil {
		return err
	}
	client, err := clobclient.New(cfg)
	if err != nil {
		return err
	}

	var calls []clobclient.WalletCall
	var summary string
	switch cmd {
	case "split":
		if conditionID == "" {
			return errors.New("--condition is required")
		}
		var market *clobclient.Market
		calls, market, err = client.SplitCalls(ctx, conditionID, amount)
		if err != nil {
			return err
		}
		summary = fmt.Sprintf("split %s pUSD into %s of each outcome of %q", clobclient.FormatUnits(amount), clobclient.FormatUnits(amount), market.Question)
		for _, t := range market.Tokens {
			summary += fmt.Sprintf("\n  %-5s %s", t.Outcome, t.TokenID)
		}
	case "send-token":
		calls, err = client.SendTokenCalls(ctx, to, tokenID, amount)
		if err != nil {
			return err
		}
		summary = fmt.Sprintf("send %s of token %s to %s", clobclient.FormatUnits(amount), tokenID, to)
	}

	fmt.Printf("deposit wallet %s (signer %s)\n%s\ncalls:\n", client.MakerAddress(), client.Address(), summary)
	for i, c := range calls {
		fmt.Printf("  %d. %s %s\n", i+1, c.Target, c.Data)
	}
	if err := client.CheckRelayer(); err != nil {
		return fmt.Errorf("cannot submit: %w", err)
	}
	if !yes {
		fmt.Println("dry run: add --yes to sign and submit")
		return nil
	}

	tx, err := client.ExecuteWalletBatch(ctx, calls, summary, time.Now().Add(10*time.Minute))
	if err != nil {
		if errors.Is(err, clobclient.ErrSubmitOutcomeUnknown) {
			// A rerun signs a fresh nonce and would execute the batch twice.
			return fmt.Errorf("%w\ncheck the deposit wallet on chain before running this again", err)
		}
		return err
	}
	fmt.Printf("submitted relayer transaction %s (%s)\n", tx.ID, tx.State)
	waitCtx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	done, err := client.WaitRelayerTransaction(waitCtx, tx.ID, 2*time.Second)
	if err != nil {
		if done != nil && done.Failed() {
			// A final failure spent nothing; running it again is safe.
			return err
		}
		return fmt.Errorf("%w\nthe batch was submitted as relayer transaction %s; check the deposit wallet on chain before running this again", err, tx.ID)
	}
	fmt.Printf("confirmed: https://polygonscan.com/tx/%s\n", done.Hash)
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: polywallet <command> [flags]

  split       --condition 0x… --amount N   turn N pUSD into N Up + N Down
  send-token  --to 0x… --token-id ID --amount N
                                           send N shares of an outcome token

Without --yes the command only prints its plan.
`)
}
