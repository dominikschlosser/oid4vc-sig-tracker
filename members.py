#!/usr/bin/env python3
"""Reads the OAuth SIG member list into sig-members.json. Members without a GitHub link can't be matched to items."""
import json, re, sys, urllib.request

URL = "https://raw.githubusercontent.com/keycloak/keycloak-oauth-sig/main/members.adoc"

try:
    adoc = urllib.request.urlopen(URL, timeout=30).read().decode()
except OSError as e:
    sys.exit(f"members.py: {e}, keeping the old sig-members.json")
members = []
for company, names in re.findall(r"^\|(.+)\n\|(.+)$", adoc, re.M):
    members += [{"login": login, "name": name, "company": company.strip()}
                for login, name in re.findall(r"https://github\.com/([\w-]+)\[([^\]]*)\]", names)]
if not members:
    sys.exit("members.py: no GitHub logins found, keeping the old sig-members.json")
json.dump({"source": "https://github.com/keycloak/keycloak-oauth-sig/blob/main/members.adoc", "members": members},
          open("sig-members.json", "w"), indent=1)
print(f"{len(members)} SIG members with a GitHub login", file=sys.stderr)
