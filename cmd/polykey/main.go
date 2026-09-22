// Command polykey manages the encrypted-at-rest signing key executiond loads
// at startup. The key is stored as keystore v3 JSON; the passphrase lives in a
// separate owner-only file.
//
//	polykey encrypt --key-file private-key.json --passphrase-file private-key.pass
//	polykey inspect --key-file private-key.json
//	polykey verify  --key-file private-key.json --passphrase-file private-key.pass
//	polykey rekey   --key-file private-key.json --passphrase-file old.pass --new-passphrase-file new.pass
//
// rekey only re-encrypts the keystore; move the new passphrase file over the
// old one afterwards, or nothing will be able to unlock the key again.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Cyvadra/polymarket-clob-client/internal/keyfile"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "polykey: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return fmt.Errorf("a subcommand is required")
	}
	switch args[0] {
	case "encrypt":
		return runEncrypt(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "rekey":
		return runRekey(args[1:])
	case "migrate":
		return runMigrate(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", args[0])
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: polykey <command> [flags]

commands:
  encrypt   encrypt a hex private key (read from stdin or prompted) into a sealed key file
  inspect   print the address a keystore file holds, without the passphrase
  verify    decrypt a keystore file and print its address; exits non-zero on failure
  rekey     re-encrypt a keystore file under a new passphrase
  migrate   convert an unsealed keystore v3 JSON from an older polykey in place

run "polykey <command> -h" for the flags of a command.
`)
}

func runEncrypt(args []string) error {
	flags := flag.NewFlagSet("encrypt", flag.ExitOnError)
	keyPath := flags.String("key-file", "", "path to write the keystore v3 JSON to (required)")
	passphraseFile := flags.String("passphrase-file", "", "file holding the passphrase; prompted for when empty")
	force := flags.Bool("force", false, "overwrite --key-file if it already exists")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keyPath) == "" {
		return fmt.Errorf("--key-file is required")
	}

	// The key never comes from a flag: that would put it in the shell history
	// and in the process list.
	privateKey, err := readSecret(os.Stdin, "private key (hex): ")
	if err != nil {
		return fmt.Errorf("read private key: %w", err)
	}
	passphrase, err := passphraseFor(*passphraseFile, "passphrase: ")
	if err != nil {
		return err
	}

	encrypted, err := keyfile.Encrypt(privateKey, passphrase, keyfile.ScryptN, keyfile.ScryptP)
	if err != nil {
		return err
	}
	if err := keyfile.Write(*keyPath, encrypted, *force); err != nil {
		return err
	}
	address, err := keyfile.Address(*keyPath)
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s for %s\n", *keyPath, address)
	return nil
}

func runInspect(args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ExitOnError)
	keyPath := flags.String("key-file", "", "path to the keystore v3 JSON (required)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keyPath) == "" {
		return fmt.Errorf("--key-file is required")
	}
	address, err := keyfile.Address(*keyPath)
	if err != nil {
		return err
	}
	fmt.Println(address)
	return nil
}

func runVerify(args []string) error {
	flags := flag.NewFlagSet("verify", flag.ExitOnError)
	keyPath := flags.String("key-file", "", "path to the keystore v3 JSON (required)")
	passphraseFile := flags.String("passphrase-file", "", "file holding the passphrase; prompted for when empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keyPath) == "" {
		return fmt.Errorf("--key-file is required")
	}
	passphrase, err := passphraseFor(*passphraseFile, "passphrase: ")
	if err != nil {
		return err
	}
	_, address, err := keyfile.Load(*keyPath, passphrase)
	if err != nil {
		return err
	}
	fmt.Println(address)
	return nil
}

func runRekey(args []string) error {
	flags := flag.NewFlagSet("rekey", flag.ExitOnError)
	keyPath := flags.String("key-file", "", "path to the keystore v3 JSON to re-encrypt in place (required)")
	passphraseFile := flags.String("passphrase-file", "", "file holding the current passphrase; prompted for when empty")
	newPassphraseFile := flags.String("new-passphrase-file", "", "file holding the new passphrase; prompted for when empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keyPath) == "" {
		return fmt.Errorf("--key-file is required")
	}
	passphrase, err := passphraseFor(*passphraseFile, "current passphrase: ")
	if err != nil {
		return err
	}
	privateKey, address, err := keyfile.Load(*keyPath, passphrase)
	if err != nil {
		return err
	}
	newPassphrase, err := passphraseFor(*newPassphraseFile, "new passphrase: ")
	if err != nil {
		return err
	}
	encrypted, err := keyfile.Encrypt(privateKey, newPassphrase, keyfile.ScryptN, keyfile.ScryptP)
	if err != nil {
		return err
	}
	// keyfile.Write overwrites through a temp file and a rename, so an
	// interrupted rekey leaves the original keystore intact rather than a
	// truncated one.
	if err := keyfile.Write(*keyPath, encrypted, true); err != nil {
		return err
	}
	fmt.Printf("re-encrypted %s for %s\n", *keyPath, address)
	// The keystore now only opens with the new passphrase, so leaving the old
	// passphrase file in place would stop the daemon from starting.
	if *passphraseFile != "" && *newPassphraseFile != "" && *passphraseFile != *newPassphraseFile {
		fmt.Printf("%s no longer unlocks it; install the new passphrase with:\n  mv %s %s\n",
			*passphraseFile, *newPassphraseFile, *passphraseFile)
	} else {
		fmt.Printf("update the passphrase file the deployment reads, or it will no longer unlock %s\n", *keyPath)
	}
	return nil
}

func runMigrate(args []string) error {
	flags := flag.NewFlagSet("migrate", flag.ExitOnError)
	keyPath := flags.String("key-file", "", "path to the unsealed keystore v3 JSON to convert in place (required)")
	passphraseFile := flags.String("passphrase-file", "", "file holding its passphrase; prompted for when empty")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*keyPath) == "" {
		return fmt.Errorf("--key-file is required")
	}
	keyJSON, err := keyfile.ReadKeyFile(*keyPath)
	if err != nil {
		return err
	}
	passphrase, err := passphraseFor(*passphraseFile, "passphrase: ")
	if err != nil {
		return err
	}
	privateKey, address, err := keyfile.DecryptLegacy(keyJSON, passphrase)
	if err != nil {
		return fmt.Errorf("%s: %w", *keyPath, err)
	}
	sealed, err := keyfile.Encrypt(privateKey, passphrase, keyfile.ScryptN, keyfile.ScryptP)
	if err != nil {
		return err
	}
	if err := keyfile.Write(*keyPath, sealed, true); err != nil {
		return err
	}
	// The passphrase is unchanged, so the existing passphrase file still works.
	fmt.Printf("sealed %s for %s\n", *keyPath, address)
	return nil
}

// passphraseFor reads the passphrase from a file, or prompts twice and
// requires the two entries to match when no file was given.
func passphraseFor(passphraseFile, prompt string) (string, error) {
	if strings.TrimSpace(passphraseFile) != "" {
		return keyfile.ReadPassphrase(passphraseFile)
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return "", fmt.Errorf("--passphrase-file is required when stdin is not a terminal")
	}
	first, err := promptSecret(prompt)
	if err != nil {
		return "", err
	}
	second, err := promptSecret("confirm " + prompt)
	if err != nil {
		return "", err
	}
	if first != second {
		return "", fmt.Errorf("passphrases do not match")
	}
	if strings.TrimSpace(first) == "" {
		return "", fmt.Errorf("passphrase must not be empty")
	}
	return first, nil
}

// readSecret takes a single line from r, prompting without echo when r is a
// terminal so the key does not end up on screen.
func readSecret(r *os.File, prompt string) (string, error) {
	if term.IsTerminal(int(r.Fd())) {
		return promptSecret(prompt)
	}
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func promptSecret(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	secret, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	return string(secret), nil
}
