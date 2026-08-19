# Local patch to go-imap

This is `github.com/emersion/go-imap/v2` at `v2.0.0-beta.8` with one change,
applied through a `replace` directive in `go.mod`.

## Tolerate malformed flags (`internal/internal.go`, `ExpectFlag`)

A real mailbox was found advertising:

```
* FLAGS (\Answered \Deleted \Draft \Flagged \Seen OneComLoadImg \\Seen NonJunk)
```

`\\Seen` — a keyword whose name begins with a backslash — is not a legal IMAP
flag. `ExpectFlag` consumes one `\`, then requires an atom, finds a second `\`,
and fails. Because flags arrive in the untagged `FLAGS` response to SELECT, the
error rejects the entire response and **the mailbox becomes impossible to open**.
One piece of junk left behind by some earlier client makes the whole mailbox
unreachable.

The patch consumes any further backslashes and preserves them in the flag name,
so the flag round-trips unchanged and everything else in the response parses.

Upstream reporting is tracked separately; when a release includes an equivalent
fix, drop `third_party/go-imap`, remove the `replace` directive, and depend on
the published module again.

To re-derive this patch against a newer beta:

    diff -r "$(go env GOMODCACHE)/github.com/emersion/go-imap/v2@<version>" third_party/go-imap
