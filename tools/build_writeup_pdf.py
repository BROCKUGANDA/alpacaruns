#!/usr/bin/env python
"""Render docs/HACKATHON_WRITEUP.md -> docs/HACKATHON_WRITEUP.pdf.

Uses a minimal markdown subset with reportlab Platypus (no native deps).

  - # / ## / ### headings
  - paragraphs
  - fenced code blocks
  - bullet lists (- or *)
  - inline `code`
  - **bold**, *italic*
  - blockquotes (> )

Styling: A4 with 18mm margins, emerald accent on h1 underline,
dark code blocks, monospace for code.
"""

from __future__ import annotations

import re
from pathlib import Path

from reportlab.lib.colors import HexColor
from reportlab.lib.enums import TA_LEFT
from reportlab.lib.pagesizes import A4
from reportlab.lib.styles import ParagraphStyle
from reportlab.lib.units import mm
from reportlab.platypus import (
    BaseDocTemplate,
    Frame,
    ListFlowable,
    ListItem,
    PageTemplate,
    Paragraph,
    Preformatted,
    Spacer,
)

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "docs" / "HACKATHON_WRITEUP.md"
DST = ROOT / "docs" / "HACKATHON_WRITEUP.pdf"

ACCENT = HexColor("#10b981")
DARK = HexColor("#0a4d3a")
CODEBG = HexColor("#0f1422")
CODEFG = HexColor("#e7ecf7")
QUOTEBG = HexColor("#f9fafb")
QUOTEFG = HexColor("#4b5563")
BORDER = HexColor("#e5e7eb")
TEXT = HexColor("#1a1f2e")


def make_styles() -> dict[str, ParagraphStyle]:
    return {
        "body": ParagraphStyle(
            "body", fontName="Helvetica", fontSize=10.5, leading=15,
            textColor=TEXT, alignment=TA_LEFT, spaceAfter=6,
        ),
        "h1": ParagraphStyle(
            "h1", fontName="Helvetica-Bold", fontSize=22, leading=26,
            textColor=DARK, spaceBefore=0, spaceAfter=10,
        ),
        "h2": ParagraphStyle(
            "h2", fontName="Helvetica-Bold", fontSize=15, leading=18,
            textColor=DARK, spaceBefore=14, spaceAfter=6,
        ),
        "h3": ParagraphStyle(
            "h3", fontName="Helvetica-Bold", fontSize=12.5, leading=15,
            textColor=HexColor("#0f766e"), spaceBefore=10, spaceAfter=4,
        ),
        "blockquote": ParagraphStyle(
            "blockquote", fontName="Helvetica-Oblique", fontSize=10,
            leading=14, textColor=QUOTEFG, leftIndent=14, rightIndent=4,
            spaceBefore=4, spaceAfter=6,
        ),
        "codeblock": ParagraphStyle(
            "codeblock", fontName="Courier", fontSize=9, leading=11,
            textColor=CODEFG, backColor=CODEBG, leftIndent=0, rightIndent=0,
            spaceBefore=4, spaceAfter=8, borderPadding=(8, 8, 8, 8),
        ),
    }


def inline_md_to_html(text: str) -> str:
    out = (
        text.replace("&", "&amp;")
        .replace("<", "&lt;")
        .replace(">", "&gt;")
    )
    out = re.sub(r"\*\*(.+?)\*\*", r"<b>\1</b>", out)
    out = re.sub(r"(?<!\*)\*([^*]+?)\*(?!\*)", r"<i>\1</i>", out)
    out = re.sub(r"`([^`]+?)`", r"<font face='Courier'>\1</font>", out)
    return out


def parse_md(md: str, styles: dict[str, ParagraphStyle]) -> list:
    flows: list = []
    lines = md.splitlines()
    i = 0
    n = len(lines)

    while i < n:
        line = lines[i]
        stripped = line.strip()

        if not stripped:
            i += 1
            continue

        # Heading
        m = re.match(r"^(#{1,6})\s+(.*)", line)
        if m:
            level = min(len(m.group(1)), 3)
            text = inline_md_to_html(m.group(2).strip())
            flows.append(Paragraph(text, styles[f"h{level}"]))
            i += 1
            continue

        # Horizontal rule
        if stripped == "---":
            i += 1
            continue

        # Code fence
        if stripped.startswith("```"):
            block = []
            i += 1
            while i < n and not lines[i].strip().startswith("```"):
                block.append(lines[i])
                i += 1
            i += 1  # closing
            code = "\n".join(block)
            flows.append(Preformatted(code, styles["codeblock"], maxLineLength=92))
            continue

        # Blockquote
        if stripped.startswith(">"):
            quote_lines = []
            while i < n and lines[i].strip().startswith(">"):
                quote_lines.append(lines[i].strip()[1:].strip())
                i += 1
            txt = inline_md_to_html(" ".join(quote_lines))
            flows.append(Paragraph(txt, styles["blockquote"]))
            continue

        # Bullet list
        if re.match(r"^[-*]\s+", stripped):
            items = []
            while i < n and re.match(r"^[-*]\s+", lines[i].strip()):
                item_text = inline_md_to_html(re.sub(r"^[-*]\s+", "", lines[i].strip()))
                items.append(ListItem(Paragraph(item_text, styles["body"]), leftIndent=12))
                i += 1
            flows.append(ListFlowable(
                items, bulletType="bullet", start="•",
                bulletFontName="Helvetica-Bold", bulletColor=ACCENT,
                leftIndent=18, spaceBefore=2, spaceAfter=6,
            ))
            continue

        # Plain paragraph
        para_lines = [line]
        i += 1
        while i < n:
            nxt = lines[i].strip()
            if not nxt:
                break
            if re.match(r"^(#{1,6})\s+", nxt):
                break
            if nxt == "---":
                break
            if nxt.startswith("```"):
                break
            if nxt.startswith(">"):
                break
            if re.match(r"^[-*]\s+", nxt):
                break
            para_lines.append(lines[i])
            i += 1
        text = " ".join(para_lines).strip()
        flows.append(Paragraph(inline_md_to_html(text), styles["body"]))

    return flows


def main() -> None:
    DST.parent.mkdir(parents=True, exist_ok=True)
    md = SRC.read_text(encoding="utf-8")
    styles = make_styles()

    doc = BaseDocTemplate(
        str(DST),
        pagesize=A4,
        leftMargin=18 * mm,
        rightMargin=16 * mm,
        topMargin=18 * mm,
        bottomMargin=18 * mm,
        title="Alpacaruns — Hackathon One-Pager",
        author="Alpacaruns",
    )
    frame = Frame(
        doc.leftMargin, doc.bottomMargin, doc.width, doc.height,
        leftPadding=0, rightPadding=0, topPadding=0, bottomPadding=0, id="main",
    )
    doc.addPageTemplates([PageTemplate(id="main", frames=[frame])])

    flows = parse_md(md, styles)
    doc.build(flows)
    size_kb = DST.stat().st_size / 1024
    print(f"wrote {DST.relative_to(ROOT)} ({size_kb:.1f} KB)")


if __name__ == "__main__":
    main()