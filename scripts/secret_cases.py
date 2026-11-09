"""Write testdata/secret_cases.json from the memory plugin's secret patterns.

The plugin's Python module is the source of truth for what counts as a
secret. The Go port in internal/scan/secrets.go has to agree with it on every
case here, including the boundary cases the plugin's lookarounds exist for,
because Go's regular expressions have no lookaround and the port does those
checks in code.

Run it with `make secret-cases` when the plugin's patterns change. When the
plugin is not checked out beside this repository, it changes nothing.

Every value below is low entropy and obviously fake: a real credential never
belongs in a test file.
"""
from __future__ import annotations

import importlib.util
import json
import re
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
PLUGIN = REPO.parent / "claude-plugins" / "plugins" / "memory" / "smrit_memory" / "secrets_scan.py"
OUT = REPO / "testdata" / "secret_cases.json"

# Python's \s for text, which the Go port spells out as a class.
WHITESPACE = [chr(c) for c in range(0x110000) if re.fullmatch(r"\s", chr(c))]

CASES = [
    # From the plugin's own tests: shapes that must be found.
    "pushed with AKIAABCDEFGHIJKLMNOP in the env",
    "token ghp_" + "a" * 36,
    "sk-ant-api03-" + "b" * 40,
    "slack xoxb-123456789012-abcdefABCDEF",
    "key AIza" + "c" * 35,
    "-----BEGIN OPENSSH PRIVATE KEY-----",
    "auth eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghijklmnop",
    "access smrit_at_" + "d" * 43,
    "refresh smrit_rt_" + "e" * 43,
    "service smrit_" + "f" * 43,
    "key sk-proj-" + "a" * 30,
    "key sk-svcacct-" + "a" * 30,
    "Authorization: Bearer " + "a1b2c3d4" * 4,
    "postgres://admin:hunter2pass@db.internal/app",
    "aws_secret_access_key = " + "A" * 40,
    "sk_live_" + "a" * 20,
    "rk_live_" + "a" * 20,
    "glpat-" + "a" * 20,
    "npm_" + "a" * 36,
    "ya29." + "a" * 30,
    "GOCSPX-" + "a" * 30,
    "xapp-1-" + "A" * 12,
    "https://hooks.slack.com/services/T000/B000/XXXX",
    "-----BEGIN PGP PRIVATE KEY BLOCK-----",
    "token: abcd1234efgh5678",
    "token = 'Zk3pQ9xV2mR7tL4wB8nY'",
    "GITHUB_TOKEN=Zk3pQ9xV2mR7tL4wB8nY",
    "access_token: Zk3pQ9xV2mR7tL4wB8nY",
    "SMRIT_MCP_TOKEN=Zk3pQ9xV2mR7tL4wB8nY",
    "API_TOKEN='Zk3pQ9xV2mR7tL4wB8nY'",
    "client secret = abcdefghijkl",
    "access key: abcdefghijkl",
    # From the plugin's own tests: ordinary work that must not be found.
    "Moved search to the hybrid ranker in api/smrit/content/search.py; 959 tests pass",
    "[tool mcp__smrit__get_document_metadata]",
    "commit " + "0123456789abcdef" * 2 + "01234567",
    "id 123e4567-e89b-12d3-a456-426614174000",
    "document Vh3kP8sWq2YtN6rLx4BzM",
    "token = secrets.token_urlsafe(32)",
    "def refresh(token: Optional[str]) -> None",
    "token: Optional[str]",
    "the task-tracker and a mask-" + "a" * 40,
    "",
    "plain prose with no credential in it at all",

    # AWS access key id: exactly sixteen upper case letters or digits.
    "AKIAABCDEFGHIJKLMNO",
    "AKIAabcdefghijklmnop",
    # AWS secret: case insensitive name, twenty non space characters.
    "AWS_SECRET_ACCESS_KEY:" + "x" * 20,
    "aws_secret_access_key = " + "x" * 19,
    "aws_ſecret_access_Key = " + "x" * 20,
    "aws_secret_access_key = " + "x" * 20,
    "aws_secret_access_key = " + "x" * 10 + "​" + "x" * 10,
    # GitHub.
    "ghp_" + "a" * 35,
    "gho_" + "a" * 36,
    "ghx_" + "a" * 36,
    "github_pat_" + "a" * 40,
    "github_pat_" + "a" * 39,
    # GitLab, npm, Anthropic.
    "glpat-" + "a" * 19,
    "npm_" + "a" * 35,
    "sk-ant-" + "a" * 19,
    # OpenAI scoped keys need a word boundary before sk.
    "sk-admin-" + "a" * 20,
    "-sk-proj-" + "a" * 20,
    "mask-proj-" + "a" * 30,
    "_sk-proj-" + "a" * 30,
    "ésk-proj-" + "a" * 30,
    "٣sk-proj-" + "a" * 30,
    "²sk-proj-" + "a" * 30,
    "́sk-proj-" + "a" * 30,
    "sk-proj-" + "a" * 19,
    # OpenAI style.
    "sk-" + "a" * 32,
    "sk-" + "a" * 31,
    "task-" + "a" * 40,
    "ésk-" + "a" * 32,
    " sk-" + "a" * 32,
    # Stripe.
    "sk_live_" + "a" * 15,
    "sk_test_" + "a" * 20,
    "pk_live_" + "a" * 20,
    # Slack.
    "xoxp-" + "1" * 10,
    "xoxc-" + "1" * 10,
    "xoxb-" + "1" * 9,
    "xapp-x-" + "A" * 12,
    "hooks.slack.com/servicesX",
    # Google.
    "AIza" + "c" * 34,
    "ya29." + "a" * 19,
    "GOCSPX-" + "a" * 19,
    # Private keys.
    "-----BEGIN RSA PRIVATE KEY-----",
    "-----BEGIN PRIVATE KEY-----",
    "-----BEGIN PUBLIC KEY-----",
    "-----BEGIN rsa PRIVATE KEY-----",

    # JWT: three segments, the first two starting eyJ, each 8 to 2048
    # characters, and no token character just before the first.
    "eyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    "eyJ" + "a" * 7 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    "eyJ" + "a" * 8 + ".eyJ" + "b" * 7 + "." + "c" * 8,
    "eyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 7,
    "eyJ" + "a" * 2048 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    "eyJ" + "a" * 2049 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    "eyJ" + "a" * 8 + ".eyJ" + "b" * 2049 + "." + "c" * 8,
    "eyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 3000,
    "xeyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    "-eyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    "_eyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    ".eyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    "éeyJ" + "a" * 8 + ".eyJ" + "b" * 8 + "." + "c" * 8,
    # A refused start followed by a good one later in the same run.
    "xeyJ" + "a" * 8 + ".eyJ" + "b" * 8 + ".eyJ" + "c" * 8 + "." + "d" * 8,
    "eyJ" + "a" * 2049 + ".eyJ" + "b" * 8 + ".eyJ" + "c" * 8 + "." + "d" * 8,
    "eyJ" * 5000,
    ("xeyJ" + "a" * 8 + ".") * 500,

    # Smrit tokens: smrit_ and forty token characters, not inside a word.
    "smrit_" + "a" * 40,
    "smrit_" + "a" * 39,
    "xsmrit_" + "a" * 43,
    "_smrit_" + "a" * 43,
    "-smrit_" + "a" * 43,
    "ésmrit_" + "a" * 43,
    "mcp__smrit__" + "a" * 40,
    "mcp__smrit__get_document_metadata " * 5,
    "xsmrit_" + "a" * 43 + " smrit_" + "b" * 40,

    # Bearer: a word boundary, Python whitespace, twenty token characters.
    "bearer " + "a" * 20,
    "BEARER\t" + "a" * 20,
    "Bearer " + "a" * 19,
    "xbearer " + "a" * 20,
    "-bearer " + "a" * 20,
    "ébearer " + "a" * 20,
    "Bearer " + "a" * 20,
    "Bearer　" + "a" * 20,
    "Bearer​" + "a" * 20,
    "Bearer " + "K" * 20,
    "Bearer " + "ı" * 20,
    "Bearer " + "İ" * 20,
    "Bearer " + "a" * 10 + "é" + "a" * 10,

    # Credentials in a URL: a scheme of at most 31 characters that does not
    # start inside a run of scheme characters, then user:password@.
    "https://user:pw@example.com",
    "Xhttps://user:pw@example.com",
    "1https://user:pw@example.com",
    ".https://user:pw@example.com",
    "Https://user:pw@example.com",
    "a" * 31 + "://user:pw@example.com",
    "a" * 32 + "://user:pw@example.com",
    "https://example.com/a:b@c",
    "https://user@example.com",
    "https://us er:pw@example.com",
    "https://user:p w@example.com",
    "https://user:p​w@example.com",
    "git+ssh://user:pw@example.com",
    "1" * 40 + "https://user:pw@example.com",

    # Passwords and keys by name.
    "password = hunter22",
    "PASSWD:abcdefgh",
    "secret=abcdefgh",
    "api_key: abcdefgh",
    "API KEY: abcdefgh",
    "apikey=abcdefgh",
    "apı_key=abcdefgh",
    "APİKEY=abcdefgh",
    "paſſword=abcdefgh",
    "access Key: abcdefgh",
    "password = short",
    "password = abcd efgh",
    "password = abcd​efgh",
    "password\n=\nabcdefgh",
    "password\x0b=\x0babcdefgh",
    "password\x1c=\x1cabcdefgh",
    "password\u0085=\u0085abcdefgh",
    "password is abcdefgh",
    "client  secret = abcdefghijkl",

    # The token rule: a value of 16 or more token characters with a letter
    # and a digit among the first 49.
    "token: " + "a" * 15 + "1",
    "token: " + "a" * 14 + "1",
    "token: abcdefghijklmnopqrst",
    "token: 1234567890123456",
    "token: " + "a" * 48 + "1",
    "token: " + "a" * 49 + "1",
    "token: " + "1" * 48 + "a",
    "token: " + "1" * 49 + "a",
    "token: " + "a" * 16 + "٣",
    "token: " + "a" * 15 + "٣",
    "token: " + "a" * 48 + "٣",
    "token: " + "a" * 49 + "٣",
    "token: " + "1" * 16 + "K",
    "token: " + "1" * 15 + "K",
    "token: " + "1" * 16 + "ı",
    "token: " + "1" * 16 + "é",
    "token: " + "1" * 15 + "éa",
    "mytoken: abcd1234efgh5678",
    "Ktoken: abcd1234efgh5678",
    "étoken: abcd1234efgh5678",
    "_token: abcd1234efgh5678",
    "TOKEN=abcd1234efgh5678",
    "toKen=abcd1234efgh5678",
    "token : abcd1234efgh5678",
    "token := abcd1234efgh5678",
    "token==abcd1234efgh567",
    "token = \"abcd1234efgh5678\"",
    "token = ''abcd1234efgh5678",
    "token=" * 2000,
    "token=1" * 2000,
    "mytoken=abcd1234efgh5678 token=x " + "token=abcd1234efgh5678",
]

# Whitespace inside a value ends it, by Python's definition of whitespace.
for ch in WHITESPACE:
    CASES.append("aws_secret_access_key = " + "x" * 10 + ch + "x" * 10)
    CASES.append("Bearer" + ch + "a" * 20)


def load():
    spec = importlib.util.spec_from_file_location("secrets_scan", PLUGIN)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def main() -> int:
    if not PLUGIN.exists():
        print(f"The plugin is not at {PLUGIN}, so {OUT.name} stays as it is.", file=sys.stderr)
        return 0
    module = load()
    compiled = module._COMPILED
    alone = [0] * len(compiled)
    seen = set()
    cases = []
    for text in CASES:
        if text in seen:
            continue
        seen.add(text)
        hits = [i for i, pattern in enumerate(compiled) if pattern.search(text)]
        if len(hits) == 1:
            alone[hits[0]] += 1
        cases.append({"text": text, "secret": module.contains_secret(text)})
    missing = [compiled[i].pattern for i, n in enumerate(alone) if n == 0]
    if missing:
        print("These patterns have no case that only they match:", *missing, sep="\n  ", file=sys.stderr)
        return 1
    negatives = sum(1 for c in cases if not c["secret"])
    if negatives == 0 or negatives == len(cases):
        print("The cases need both secrets and ordinary text.", file=sys.stderr)
        return 1
    OUT.write_text(json.dumps(cases, indent=1) + "\n")
    print(f"Wrote {len(cases)} cases, {len(cases) - negatives} secrets, to {OUT.relative_to(REPO)}.")
    return 0


if __name__ == "__main__":
    sys.exit(main())
