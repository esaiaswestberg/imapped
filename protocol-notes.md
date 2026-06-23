# Protocol Notes

This document records what the current IMAP frontend advertises and the command surface it currently implements.

## Advertised Now

- `IMAP4rev1`
- `STARTTLS`
- `AUTH=PLAIN`
- `AUTH=XOAUTH2`
- `UIDPLUS`
- `NAMESPACE`
- `SPECIAL-USE`
- `LIST-STATUS`
- `IDLE`
- `CONDSTORE`
- `ENABLE`
- `ID`
- `ESEARCH`
- `MOVE`
- `SORT`
- `THREAD=REFERENCES`
- `THREAD=ORDEREDSUBJECT`
- `UNSELECT`

Capability advertisement is state-dependent. For example, `STARTTLS` is only advertised while the connection is still in cleartext, and the authenticated capability set is larger than the unauthenticated one.

## Implemented Command Surface

- `CAPABILITY`
- `NOOP`
- `LOGOUT`
- `STARTTLS`
- `LOGIN`
- `AUTHENTICATE`
- `SELECT`
- `EXAMINE`
- `CREATE`
- `DELETE`
- `RENAME`
- `SUBSCRIBE`
- `UNSUBSCRIBE`
- `LIST`
- `LSUB`
- `STATUS`
- `APPEND`
- `CHECK`
- `CLOSE`
- `EXPUNGE`
- `SEARCH`
- `FETCH`
- `STORE`
- `COPY`
- `MOVE`
- `UID`
- `IDLE`
- `UNSELECT`
- `NAMESPACE`
- `ENABLE`
- `CONDSTORE`
- `ID`
- `THREAD`
- `LIST-STATUS`

## Implementation Notes

- `FETCH` supports raw RFC822 reads, partial fetches, and common metadata items.
- `SEARCH` uses PostgreSQL for structured fields and Tantivy for text-oriented queries.
- The sync engine preserves raw message bytes and uses content-addressed object storage for large payloads.
- Flags are reconciled from upstream state so local mailbox metadata does not drift silently.
- Live upstream tests use real credentials from `.testing-credentials` and are serialized so they do not race each other across test binaries.

## Remaining Gaps

The current implementation is intentionally scoped around the core caching-mirror workflow. Features not advertised above should remain unadvertised until they are implemented and verified end to end.

In particular, if you add more advanced IMAP extensions later, update this file at the same time as the code and tests so the docs do not drift from the runtime behavior.
