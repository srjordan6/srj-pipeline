#!/usr/bin/env python3
"""SRJ training-corpus export.

Runs daily on Render (cron srj-corpus-export, 11:30 UTC, after the 11:00
intel/pipeline syncs) and snapshots everything the practice publishes into a
JSONL corpus on Cloudflare R2, for LLM fine-tuning and retrieval.

One JSON record per item:
  {id, type, title, text, url, source, provenance, created, content_hash}

Provenance gates what may be trained on:
  owned         Stephen's authored content. Safe to train on.
  public-record Court dockets and filings metadata. Safe to train on.
  third-party   Vendor/news headlines discovered by the watchers. Retrieval
                only; excluded from training sets by default.

Output keys (private srj-uploads bucket, via the Worker's PUT /api/archive,
bearer-gated by ARCHIVE_TOKEN; no direct R2 credentials needed):
  corpus/training/YYYY-MM-DD/corpus.jsonl   immutable daily snapshot
  corpus/training/YYYY-MM-DD/manifest.json  counts, hash, byte size
  corpus/training/latest/corpus.jsonl       rolling pointer for consumers
  corpus/training/latest/manifest.json

Required env: DATABASE_URL, ARCHIVE_TOKEN.
Every run logs a row to srj_corpus_log, success or failure.
"""
import hashlib
import io
import json
import os
import sys
import tarfile
from datetime import datetime, timezone

import psycopg2
import psycopg2.extras
import requests

ARCHIVE_URL = "https://srjconsultingservices.com/api/archive"
CONTENT_TARBALL = "https://codeload.github.com/srjordan6/srj-content/tar.gz/refs/heads/main"
SITE = "https://srjconsultingservices.com"

# srj-content top-level dir -> (record type, provenance)
CONTENT_DIRS = {
    "governance": ("governance-page", "owned"),
    "news": ("news", "owned"),
    "people": ("person-profile", "owned"),
    "resources": ("resource", "owned"),
    "books": ("book", "owned"),
    "lawsuits": ("lawsuit-dossier", "owned"),
    "legislation": ("legislation", "owned"),
    "migrated": ("site-page", "owned"),
    "leaderboard": ("leaderboard", "owned"),
    "intel": ("vendor-intel", "third-party"),
}

# Postgres table -> (record type, provenance)
DB_TABLES = {
    "synced_glossary_terms": ("glossary-term", "owned"),
    "synced_laws": ("law-summary", "owned"),
    "synced_tools": ("tool-entry", "owned"),
    "ai_lawsuits": ("lawsuit-tracked", "public-record"),
    "ai_lawsuit_candidates": ("lawsuit-candidate", "public-record"),
    "ai_intel_candidates": ("intel-candidate", "third-party"),
}


def norm(v):
    if isinstance(v, datetime):
        return v.isoformat()
    return v


def row_record(table, rtype, prov, row):
    d = {k: norm(v) for k, v in row.items() if v not in (None, "", [])}
    title = None
    for k in ("title", "name", "term", "case_name", "law_name", "tool_name", "page"):
        if d.get(k):
            title = str(d[k])
            break
    # text: concatenate the substantive string fields, longest first
    strings = sorted(
        (str(v) for k, v in d.items() if isinstance(v, str) and len(str(v)) > 40),
        key=len,
        reverse=True,
    )
    text = "\n\n".join(strings) if strings else json.dumps(d, ensure_ascii=False, default=str)
    rid = d.get("id") or d.get("slug") or hashlib.md5(json.dumps(d, sort_keys=True, default=str).encode()).hexdigest()[:12]
    url = d.get("url") or d.get("canonical_url")
    created = d.get("created_at") or d.get("discovered_at") or d.get("date_created")
    return {
        "id": f"db:{table}:{rid}",
        "type": rtype,
        "title": title,
        "text": text,
        "url": url,
        "source": f"postgres:{table}",
        "provenance": prov,
        "created": created,
        "raw": d,
    }


def db_records(dsn):
    out = []
    with psycopg2.connect(dsn) as conn:
        with conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor) as cur:
            for table, (rtype, prov) in DB_TABLES.items():
                cur.execute(f"SELECT * FROM {table}")
                for row in cur.fetchall():
                    out.append(row_record(table, rtype, prov, row))
    return out


def json_items(obj):
    """A content file may be one object, a list, or {items|cases|entries: [...]}."""
    if isinstance(obj, list):
        return obj
    if isinstance(obj, dict):
        for key in ("items", "cases", "entries", "terms", "tools", "laws", "pages", "people"):
            if isinstance(obj.get(key), list):
                return obj[key]
        return [obj]
    return []


def content_records():
    out = []
    resp = requests.get(CONTENT_TARBALL, timeout=120)
    resp.raise_for_status()
    tf = tarfile.open(fileobj=io.BytesIO(resp.content), mode="r:gz")
    for member in tf.getmembers():
        parts = member.name.split("/", 2)  # srj-content-main/<dir>/<file>
        if len(parts) < 3 or not member.isfile() or not member.name.endswith(".json"):
            continue
        top = parts[1]
        if top not in CONTENT_DIRS:
            continue
        rtype, prov = CONTENT_DIRS[top]
        try:
            data = json.load(tf.extractfile(member))
        except Exception:
            continue
        for i, item in enumerate(json_items(data)):
            if not isinstance(item, dict):
                continue
            d = {k: norm(v) for k, v in item.items() if v not in (None, "", [])}
            title = next((str(d[k]) for k in ("title", "name", "term", "case_name", "headline") if d.get(k)), None)
            strings = sorted(
                (str(v) for v in d.values() if isinstance(v, str) and len(str(v)) > 40),
                key=len,
                reverse=True,
            )
            text = "\n\n".join(strings) if strings else json.dumps(d, ensure_ascii=False)
            rid = d.get("slug") or d.get("id") or f"{parts[2]}:{i}"
            url = d.get("url")
            if not url and d.get("slug") and top == "governance":
                url = f"{SITE}/ai-governance/{d['slug']}/"
            out.append({
                "id": f"content:{top}:{rid}",
                "type": rtype,
                "title": title,
                "text": text,
                "url": url,
                "source": f"srj-content:{member.name.split('/', 1)[1]}",
                "provenance": prov,
                "created": d.get("updated") or d.get("date") or d.get("generated"),
                "raw": d,
            })
    return out


def log_run(dsn, ok, counts=None, size=0, digest=None, key=None, detail=None):
    try:
        with psycopg2.connect(dsn) as conn, conn.cursor() as cur:
            c = counts or {}
            cur.execute(
                "INSERT INTO srj_corpus_log (ok, records, owned_records, public_records, "
                "thirdparty_records, bytes, sha256, r2_key, detail) VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s)",
                (ok, c.get("total", 0), c.get("owned", 0), c.get("public-record", 0),
                 c.get("third-party", 0), size, digest, key, detail),
            )
    except Exception as e:  # logging must never mask the real failure
        print(f"corpus-log write failed: {e}", file=sys.stderr)


def main():
    dsn = os.environ["DATABASE_URL"]
    try:
        records = db_records(dsn) + content_records()
        counts = {"total": len(records), "owned": 0, "public-record": 0, "third-party": 0}
        lines = []
        for r in records:
            counts[r["provenance"]] += 1
            r["content_hash"] = hashlib.sha256((r["text"] or "").encode()).hexdigest()[:16]
            lines.append(json.dumps(r, ensure_ascii=False, default=str))
        body = ("\n".join(lines) + "\n").encode()
        digest = hashlib.sha256(body).hexdigest()
        day = datetime.now(timezone.utc).strftime("%Y-%m-%d")
        manifest = json.dumps({
            "generated": datetime.now(timezone.utc).isoformat(),
            "records": counts, "bytes": len(body), "sha256": digest,
            "training_note": "Train on provenance in {owned, public-record} only; "
                             "third-party records are retrieval-only.",
        }, indent=2).encode()

        token = os.environ["ARCHIVE_TOKEN"]
        headers = {"authorization": f"Bearer {token}"}
        for prefix in (f"corpus/training/{day}", "corpus/training/latest"):
            for name, payload, ctype in (("corpus.jsonl", body, "application/x-ndjson"),
                                          ("manifest.json", manifest, "application/json")):
                r = requests.put(f"{ARCHIVE_URL}?key={prefix}/{name}", data=payload,
                                 headers={**headers, "content-type": ctype}, timeout=300)
                r.raise_for_status()

        log_run(dsn, True, counts, len(body), digest, f"corpus/training/{day}/corpus.jsonl")
        print(f"corpus export ok: {counts} -> srj-uploads:corpus/training/{day}/ ({len(body)} bytes)")
    except Exception as e:
        log_run(dsn, False, detail=str(e)[:500])
        raise


if __name__ == "__main__":
    main()
