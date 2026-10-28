#!/usr/bin/env python3
"""Prepare/publish quota events through Multica CLI. Not installed by this PR.

Input is normalized JSON: account, observed_at, fresh_until, five_hour and
seven_day (remaining_percent, reset_at). A local durable ledger freezes the first
payload for each semantic identity, making retries byte-stable across restarts.
No issue creation or Telegram/Slack client exists in this adapter.
"""
import argparse
import datetime as dt
import fcntl
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile


def timestamp(value):
    parsed = dt.datetime.fromisoformat(value.replace("Z", "+00:00"))
    if parsed.tzinfo is None:
        raise ValueError("timestamps require a timezone")
    return parsed.astimezone(dt.timezone.utc).isoformat().replace("+00:00", "Z")


def events(snapshot):
    account = snapshot["account"]
    if not isinstance(account, str) or not account or "/" in account or len(account) > 100:
        raise ValueError("invalid account")
    observed = timestamp(snapshot["observed_at"])
    fresh = timestamp(snapshot["fresh_until"])
    quota = {}
    for window in ("five_hour", "seven_day"):
        raw = snapshot.get(window, {})
        remaining = raw.get("remaining_percent")
        if remaining is not None and (isinstance(remaining, bool) or not isinstance(remaining, (int, float)) or not 0 <= remaining <= 100):
            raise ValueError("invalid remaining percentage")
        quota[window] = {"remaining_percent": remaining, "reset_at": timestamp(raw["reset_at"])}
    for window, data in quota.items():
        remaining, reset = data["remaining_percent"], data["reset_at"]
        action = "blocked_until_reset" if remaining == 0 else "source_unavailable" if remaining is None else None
        if action is None:
            continue
        if dt.datetime.fromisoformat(observed.replace("Z", "+00:00")) >= dt.datetime.fromisoformat(reset.replace("Z", "+00:00")):
            action = "reset_not_confirmed"
        subject = f"claude/{account}/{window}"
        identity = json.dumps([subject, reset, action], separators=(",", ":"))
        yield {"event_key": hashlib.sha256(identity.encode()).hexdigest(), "producer": "subscription-limits",
               "facts": {"subject": subject, "period": reset, "action": action,
                         "observed_at": observed, "reset_at": reset, "fresh_until": fresh,
                         "urgency": "routine" if action == "blocked_until_reset" else "unknown"}, "claude": quota}


def publish(snapshot, ledger_path, cli="multica", execute=False, agent_id=None):
    ledger_path = Path(ledger_path)
    ledger_path.parent.mkdir(parents=True, exist_ok=True)
    # A separate stable lock inode survives atomic replacement of the ledger.
    with open(str(ledger_path) + ".lock", "a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        ledger = json.loads(ledger_path.read_text()) if ledger_path.exists() else {}
        prepared = []
        for event in events(snapshot):
            stored = ledger.setdefault(event["event_key"], event)
            prepared.append(stored)
        descriptor, temp = tempfile.mkstemp(dir=ledger_path.parent, prefix=".quota-ledger-")
        try:
            with os.fdopen(descriptor, "w") as output:
                json.dump(ledger, output, ensure_ascii=False, allow_nan=False)
                output.flush()
                os.fsync(output.fileno())
            os.replace(temp, ledger_path)
            directory = os.open(ledger_path.parent, os.O_RDONLY)
            try:
                os.fsync(directory)
            finally:
                os.close(directory)
        finally:
            if os.path.exists(temp):
                os.unlink(temp)
        if execute:
            # Persisted payloads are retried unchanged even after a lost CLI result.
            for event in prepared:
                with tempfile.NamedTemporaryFile(mode="w", encoding="utf-8", dir=ledger_path.parent, suffix=".json") as body:
                    json.dump(event, body, ensure_ascii=False, allow_nan=False)
                    body.flush()
                    command = [cli, "notification", "publish", "--file", body.name, "--output", "json"]
                    if agent_id:
                        command.extend(["--agent-id", agent_id])
                    subprocess.run(command, check=True)
        return prepared


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--snapshot", required=True)
    parser.add_argument("--ledger", required=True)
    parser.add_argument("--publish", action="store_true", help="Call Multica CLI; default prepares locally only")
    parser.add_argument("--agent-id", help="Required for human CLI context; task context derives its agent")
    args = parser.parse_args()
    print(json.dumps(publish(json.loads(Path(args.snapshot).read_text()), args.ledger, execute=args.publish, agent_id=args.agent_id), ensure_ascii=False))


if __name__ == "__main__":
    main()
