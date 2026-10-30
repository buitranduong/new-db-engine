#!/usr/bin/env python3
"""Resolve merge conflicts in docs/*.md by keeping both sides, and renumber the
sections the incoming side added when their number or appendix letter is
already taken ("## 25." -> next free number, "## Phụ lục E." -> next letter).
Usage: scripts/merge-docs.py <theirs-name> file.md [file.md ...]
Only .md files; code conflicts are never touched."""
import re, sys

def resolve(path, theirs):
    s = open(path).read()
    pos, out, n = 0, [], 0
    while True:
        i = s.find("<<<<<<< ", pos)
        if i < 0:
            out.append(s[pos:]); break
        i2 = s.index("\n", i) + 1
        m = s.index("\n=======\n", i2)
        j = s.index("\n>>>>>>> ", m)
        j2 = s.index("\n", j + 1) + 1
        head, other = s[i2:m + 1], s[m + 9:j + 1]
        out.append(s[pos:i]); out.append(head); out.append(("\n" if not head.endswith("\n\n") else "") + other)
        pos = j2; n += 1
    return "".join(out), n

def renumber(s, other_start_markers):
    """Renumber headings that appear more than once (the later occurrence is the incoming one)."""
    lines = s.split("\n")
    seen_num, seen_app = set(), set()
    nums = [int(m.group(1)) for l in lines if (m := re.match(r"## (\d+)\.", l))]
    nxt = (max(nums) if nums else 0) + 1
    letters = [m.group(1) for l in lines if (m := re.match(r"## Phụ lục ([A-Z])\.", l))]
    nxtl = chr(ord(max(letters)) + 1) if letters else "A"
    ren = {}
    for k, l in enumerate(lines):
        m = re.match(r"## (\d+)\.(.*)", l)
        if m:
            num = int(m.group(1))
            if num in seen_num:
                lines[k] = f"## {nxt}.{m.group(2)}"; ren[f"§{num}"] = f"§{nxt}"; nxt += 1
            else:
                seen_num.add(num)
            continue
        m = re.match(r"## Phụ lục ([A-Z])\.(.*)", l)
        if m:
            if m.group(1) in seen_app:
                lines[k] = f"## Phụ lục {nxtl}.{m.group(2)}"; nxtl = chr(ord(nxtl) + 1)
            else:
                seen_app.add(m.group(1))
    return "\n".join(lines)

if __name__ == "__main__":
    theirs = sys.argv[1]
    for p in sys.argv[2:]:
        if not p.endswith(".md"):
            print("skip (not markdown):", p); continue
        s, n = resolve(p, theirs)
        s = renumber(s, None)
        assert "<<<<<<<" not in s and ">>>>>>>" not in s, p
        open(p, "w").write(s)
        print(f"{p}: {n} conflict block(s) resolved (both sides kept)")
