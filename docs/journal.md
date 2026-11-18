# The helper's journal

Every change the helper makes is recorded in `/var/lib/snugbox-helper/journal`, readable only by root. Each step of a fix is one line, and each line holds the SHA-256 of the line before it, so no line can be edited, removed or moved afterwards without the check finding it. The helper checks the whole journal every time it starts.

## What a line holds

Each line is one JSON object:

| Field | Meaning |
| --- | --- |
| `seq` | The line number, from 1 |
| `time` | When the step was recorded, in UTC; it never goes back, even if the clock does |
| `tx` | The transaction, such as `20261031-103015-service-limits`, its start time in UTC and the fix |
| `state` | The step: `approved`, `preflight`, `snapshot`, `applying`, `verifying`, `watching`, `done`, `reopened`, `undoing`, `undone` or `failed` |
| `by` | Who took the step: `root` or `snugboxd` for what they asked for, and `helper` for the steps it takes on its own, such as the preflight checks, the snapshot, or finishing a transaction after a restart |
| `note` | Why, in words, when there is something to say |
| `data` | The fix as asked for, or the list of changes with their old values |
| `prev` | The `hash` of the line before; the first line has 64 zeros |
| `hash` | The SHA-256 of this line without its `hash` field |

The hash covers the line's exact bytes, so even a change that reads the same as JSON is found. The snapshot that undo restores from, `/var/lib/snugbox-helper/undo/<transaction>/manifest.json`, has its own SHA-256 in the journal's `snapshot` line, so a changed snapshot is found too.

## Checking it yourself

You don't have to trust Snugbox to check the chain. As root, with Python 3:

```python
import hashlib, json

prev = "0" * 64
with open("/var/lib/snugbox-helper/journal", "rb") as journal:
    for n, line in enumerate(journal, 1):
        body, _, rest = line.rstrip(b"\n").rpartition(b',"hash":"')
        body += b"}"
        digest = rest[:-2].decode()
        entry = json.loads(body)
        assert hashlib.sha256(body).hexdigest() == digest, f"line {n} was changed"
        assert entry["seq"] == n and entry["prev"] == prev, f"a line before {n} is missing or moved"
        prev = digest
print(f"{n} lines, chain intact, last hash {prev}")
```

## When the check fails

The helper then changes nothing at all, and every request gets an error naming the line and the problem; see [when the helper is stuck](fixes/stuck.md).

A last line cut short, because the machine stopped in the middle of writing it, is not a failure: the helper removes the partial line, logs that it did, and finishes or undoes the transaction it belonged to.

The journal and the snapshots stay when Snugbox is removed, even by a purge, because a fix still in place needs them to be undone.
