package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/jamshids/rillock/internal/auth"
	"github.com/jamshids/rillock/internal/config"
)

func runKeys(_ context.Context, a *app, fs *flag.FlagSet, args []string) error {
	name := fs.String("name", "", "name recorded when this key is used, such as your user name (required)")
	roles := fs.String("roles", "", "comma-separated roles: admin, approver, viewer (required)")
	keysFile := fs.String("keys-file", "", "add the key to this keys file (created if missing)")
	rest, err := config.Parse(fs, args, a.env)
	if err != nil {
		return err
	}
	if len(rest) != 1 || rest[0] != "generate" {
		return usageErrorf("the only keys subcommand is generate")
	}
	if *name == "" || *roles == "" {
		return usageErrorf("--name and --roles are required")
	}

	entry := auth.Entry{Name: *name}
	for r := range strings.SplitSeq(*roles, ",") {
		entry.Roles = append(entry.Roles, auth.Role(strings.TrimSpace(r)))
	}
	key, err := auth.Generate()
	if err != nil {
		return err
	}
	entry.SHA256 = auth.Hash(key)
	if err := entry.Validate(); err != nil {
		return err
	}

	if *keysFile != "" {
		if err := auth.AddToFile(*keysFile, entry); err != nil {
			return err
		}
	}

	fmt.Fprintf(a.stdout, "API key for %s (roles: %s):\n\n  %s\n\n", entry.Name, *roles, key)
	fmt.Fprintln(a.stdout, "This is the only time the key is shown. Use it with:")
	fmt.Fprintf(a.stdout, "  export RILLOCK_API_KEY=%s\n\n", key)
	if *keysFile != "" {
		fmt.Fprintf(a.stdout, "Added to %s. Load it into a running server by sending it SIGHUP (make reload-keys).\n", *keysFile)
		return nil
	}
	fmt.Fprintln(a.stdout, "Add this entry to the server's keys file:")
	fmt.Fprintf(a.stdout, "  - name: %s\n    roles: [%s]\n    sha256: %s\n", entry.Name, strings.Join(roleNames(entry.Roles), ", "), entry.SHA256)
	return nil
}

func roleNames(roles []auth.Role) []string {
	out := make([]string, len(roles))
	for i, r := range roles {
		out[i] = string(r)
	}
	return out
}
