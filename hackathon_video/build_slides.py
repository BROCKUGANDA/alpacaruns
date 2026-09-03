#!/usr/bin/env python3
"""Render Alpacaruns hackathon pitch slides as 1280x720 PNGs."""
from __future__ import annotations
import os
from PIL import Image, ImageDraw, ImageFont

OUT_DIR = r"C:/Users/HP/Desktop/Alpacaruns/hackathon_video/slides"
os.makedirs(OUT_DIR, exist_ok=True)

W, H = 1280, 720

BG_TOP    = (16, 22, 36)
BG_BOT    = (24, 32, 50)
ACCENT    = (255, 184, 92)
ACCENT2   = (120, 220, 200)
TEXT      = (240, 244, 250)
MUTED     = (170, 180, 200)
DIVIDER   = (60, 75, 100)

def font(size: int, bold: bool = False) -> ImageFont.ImageFont:
    candidates = [
        r"C:\Windows\Fonts\segoeuib.ttf" if bold else r"C:\Windows\Fonts\segoeui.ttf",
        r"C:\Windows\Fonts\arialbd.ttf" if bold else r"C:\Windows\Fonts\arial.ttf",
    ]
    for path in candidates:
        if os.path.exists(path):
            return ImageFont.truetype(path, size)
    return ImageFont.load_default()

F_TITLE = font(54, bold=True)
F_SUB   = font(28)
F_BODY  = font(26)
F_BULLET = font(24)
F_TAG   = font(18)
F_CODE  = font(20)

def gradient_bg() -> Image.Image:
    img = Image.new("RGB", (W, H), BG_TOP)
    px = img.load()
    for y in range(H):
        t = y / H
        r = int(BG_TOP[0] * (1 - t) + BG_BOT[0] * t)
        g = int(BG_TOP[1] * (1 - t) + BG_BOT[1] * t)
        b = int(BG_TOP[2] * (1 - t) + BG_BOT[2] * t)
        for x in range(W):
            px[x, y] = (r, g, b)
    return img

def header(d: ImageDraw.ImageDraw, slide_num: int, total: int, tag: str):
    d.rectangle([(0, 0), (W, 60)], fill=(10, 14, 22))
    d.text((80, 18), "ALPACARUNS  ·  Hackathon Pitch", font=F_TAG, fill=MUTED)
    d.text((W - 200, 18), f"Slide {slide_num} / {total}", font=F_TAG, fill=MUTED)
    d.rectangle([(0, 60), (W, 62)], fill=DIVIDER)
    if tag:
        d.text((80, 75), tag.upper(), font=F_TAG, fill=ACCENT)

def footer(d: ImageDraw.ImageDraw, line: str):
    d.rectangle([(0, H - 50), (W, H)], fill=(10, 14, 22))
    d.text((80, H - 36), line, font=F_TAG, fill=MUTED)

def wrap_to_width(text: str, font: ImageFont.ImageFont, max_px: int, draw: ImageDraw.ImageDraw) -> list[str]:
    words = text.split()
    lines, current = [], ""
    for w in words:
        candidate = (current + " " + w).strip()
        bbox = draw.textbbox((0, 0), candidate, font=font)
        if bbox[2] - bbox[0] <= max_px:
            current = candidate
        else:
            if current:
                lines.append(current)
            current = w
    if current:
        lines.append(current)
    return lines

def bullet_block(d, x, y, items, font, max_w=1040, item_gap=18):
    cur_y = y
    for item in items:
        d.ellipse([(x, cur_y + 14), (x + 12, cur_y + 26)], fill=ACCENT)
        wrapped = wrap_to_width(item, font, max_w - 40, d)
        for i, line in enumerate(wrapped):
            d.text((x + 32, cur_y if i == 0 else cur_y + 36 * i), line, font=font, fill=TEXT)
        cur_y += 36 * len(wrapped) + item_gap
    return cur_y

TOTAL = 7

def slide1():
    img = gradient_bg()
    d = ImageDraw.Draw(img)
    d.rectangle([(0, 0), (12, H)], fill=ACCENT)
    d.text((100, 200), "ALPACARUNS", font=font(80, bold=True), fill=TEXT)
    d.text((100, 300), "Autonomous MoE Multi-Agent Trading on Alpaca", font=font(34), fill=ACCENT2)
    d.text((100, 360), "Go  ·  Google ADK  ·  Alpaca Trading API  ·  MCP", font=F_SUB, fill=MUTED)
    d.rectangle([(90, 460), (W - 90, 600)], outline=ACCENT, width=3)
    d.text((115, 480), "A trading bot that is auditable end-to-end —", font=F_BODY, fill=TEXT)
    d.text((115, 520), "five factor voices, one risk validator, one journal.", font=F_BODY, fill=TEXT)
    d.text((115, 560), "Paper-only by default.  Fail-closed.  No silent trades.", font=F_SUB, fill=ACCENT)
    d.text((100, H - 90), "Hackathon submission  ·  Sept 2026", font=F_TAG, fill=MUTED)
    img.save(os.path.join(OUT_DIR, "slide_01.png"))

def slide2():
    img = gradient_bg()
    d = ImageDraw.Draw(img)
    header(d, 2, TOTAL, "Problem")
    d.text((100, 130), "Trading bots should never be opaque.", font=F_TITLE, fill=TEXT)
    bullets = [
        "Most retail \"AI trading\" demos hide what they actually decided and why.",
        "LLM-driven agents fail silently: malformed tool calls, missing prices, drifted state.",
        "Alpaca's free tier blocks options snapshots — naive bots break; smart bots degrade.",
        "Owners deserve one journal that explains every fill, HOLD, and halt.",
    ]
    bullet_block(d, 100, 240, bullets, font=F_BULLET)
    footer(d, "Alpacaruns is built so every order is reproducible and explainable.")
    img.save(os.path.join(OUT_DIR, "slide_02.png"))

def slide3():
    img = gradient_bg()
    d = ImageDraw.Draw(img)
    header(d, 3, TOTAL, "What it does")
    d.text((100, 130), "Two execution paths.  One shared risk gate.", font=F_TITLE, fill=TEXT)
    d.text((100, 230), "LLM path  (cycle / monitor)", font=F_SUB, fill=ACCENT)
    llm_bullets = [
        "SequentialAgent: Market → Analysis(Technical ∥ Sentiment) → TradeIdea → Risk → Execution.",
        "Each Alpaca call is a tool from the official MCP server — no hand-rolled REST.",
        "Supervises live decisions; human approval unless MODE=autonomous AND confidence clears.",
    ]
    bullet_block(d, 100, 280, llm_bullets, font=F_BULLET, max_w=520)
    d.text((680, 230), "Deterministic path  (auto)", font=F_SUB, fill=ACCENT2)
    auto_bullets = [
        "Pure pipeline: bars → five factors → threshold rule → size → risk → order.",
        "Identical inputs always produce identical outputs.  No LLM, no surprise.",
        "Optional ensemble: six expert voices with performance-weighted gater.",
    ]
    bullet_block(d, 680, 280, auto_bullets, font=F_BULLET, max_w=540)
    footer(d, "Both paths funnel every order through the same Go-coded pre-trade gate.")
    img.save(os.path.join(OUT_DIR, "slide_03.png"))

def slide4():
    img = gradient_bg()
    d = ImageDraw.Draw(img)
    header(d, 4, TOTAL, "Architecture")
    d.text((100, 130), "Mixture-of-Experts graph on Google ADK for Go.", font=F_TITLE, fill=TEXT)

    boxes = [
        (100, 250, 240, 110, "GatingRoot\n(LLM router)", ACCENT),
        (380, 250, 240, 110, "MarketData\nExpert", ACCENT2),
        (660, 250, 240, 110, "Analysis\n(Technical ∥ News)", ACCENT2),
        (940, 250, 240, 110, "TradeIdea\nExpert", ACCENT2),
        (100, 420, 240, 110, "Risk Mgmt\nExpert", ACCENT),
        (380, 420, 240, 110, "Execution\nExpert (MCP)", ACCENT),
        (660, 420, 240, 110, "Alpaca\nPaper API", (180, 200, 220)),
        (940, 420, 240, 110, "Journal\n(JSONL)", (180, 200, 220)),
    ]
    for x, y, w, h, label, color in boxes:
        d.rectangle([(x, y), (x + w, y + h)], outline=color, width=3)
        for i, line in enumerate(label.split("\n")):
            f = F_BODY if i == 0 else F_TAG
            d.text((x + 16, y + 18 + i * 30), line, font=f, fill=TEXT)

    arrow_pairs = [
        ((340, 305), (380, 305)),
        ((620, 305), (660, 305)),
        ((900, 305), (940, 305)),
        ((340, 475), (380, 475)),
        ((620, 475), (660, 475)),
        ((900, 475), (940, 475)),
    ]
    for (x1, y1), (x2, y2) in arrow_pairs:
        d.line([(x1, y1), (x2, y2)], fill=MUTED, width=3)
        d.polygon([(x2, y2 - 6), (x2 + 12, y2), (x2, y2 + 6)], fill=MUTED)

    d.line([(220, 420), (220, 380), (1060, 380), (1060, 360)], fill=DIVIDER, width=2)
    d.text((1080, 350), "loop until halt", font=F_TAG, fill=MUTED)

    footer(d, "Six ADK experts.  One shared validator.  One JSONL journal.")
    img.save(os.path.join(OUT_DIR, "slide_04.png"))

def slide5():
    img = gradient_bg()
    d = ImageDraw.Draw(img)
    header(d, 5, TOTAL, "Risk Controls")
    d.text((100, 130), "Fail-closed.  One validator.  Every order path.", font=F_TITLE, fill=TEXT)
    items = [
        "Position notional cap (MAX_POSITION_USD) and portfolio-percentage cap (MAX_PORTFOLIO_PCT).",
        "Multi-factor gate: composite score ≥ FACTOR_MIN_SCORE; trend + momentum thresholds.",
        "Confidence floor: MIN_CONFIDENCE; ensemble needs ≥ 0.55 with a 2:1 directional split.",
        "Drawdown halts: daily 5%, weekly 10%, total 15% of peak — kill switch engages before any entry.",
        "Idempotent client_order_ids: no duplicate market-sells after a network timeout.",
    ]
    bullet_block(d, 100, 230, items, font=F_BULLET, item_gap=22)
    footer(d, "Hardening audit (Aug 26) verified all gates live; two money-path defects fixed.")
    img.save(os.path.join(OUT_DIR, "slide_05.png"))

def slide6():
    img = gradient_bg()
    d = ImageDraw.Draw(img)
    header(d, 6, TOTAL, "Tech stack")
    d.text((100, 130), "What we built it with.", font=F_TITLE, fill=TEXT)
    cells = [
        ("Language",    "Go 1.22+"),
        ("Agent graph", "Google ADK for Go (Sequential / Parallel / Loop)"),
        ("Broker",      "Alpaca Trading API via official MCP server (uvx alpaca-mcp-server)"),
        ("LLM",         "Qwen3-4B-Instruct via llama.cpp (--jinja)  ·  or Gemini  ·  or gpt-oss-20b on Oxlo"),
        ("State",       "JSONL journal + versioned strategy-state.json, temp+rename writes"),
        ("Deploy",      "Distroless Docker + systemd unit, non-root, ProtectSystem=strict"),
    ]
    y = 230
    for label, value in cells:
        d.rectangle([(100, y), (280, y + 56)], outline=ACCENT, width=2)
        d.text((118, y + 18), label, font=F_SUB, fill=ACCENT)
        wrapped = wrap_to_width(value, F_BODY, 880, d)
        for i, line in enumerate(wrapped):
            d.text((310, y + 12 + i * 28), line, font=F_BODY, fill=TEXT)
        y += 64
    footer(d, "One binary.  No cloud account required to run paper.")
    img.save(os.path.join(OUT_DIR, "slide_06.png"))

def slide7():
    img = gradient_bg()
    d = ImageDraw.Draw(img)
    header(d, 7, TOTAL, "Honest constraints + demo")
    d.text((100, 130), "What we are not claiming.  Live journal.", font=F_TITLE, fill=TEXT)

    # left: constraints
    d.text((100, 200), "Honest constraints", font=F_SUB, fill=ACCENT)
    constraints = [
        "Paper-only so far — no live capital traded.",
        "Options overlay degrades to equity on free tier (404s).",
        "No backtester yet — thresholds validated by tests + paper runs.",
        "Two OPEN items: webhook notifier, journal rotation.",
    ]
    bullet_block(d, 100, 240, constraints, font=F_BULLET, max_w=560, item_gap=16)

    # right: demo code
    d.text((720, 200), "Live journal", font=F_SUB, fill=ACCENT2)
    code_lines = [
        ('buy TSLA qty=5 price=360.98', TEXT),
        ('  tp=375.42 sl=353.76 crypto=false', TEXT),
        ('', TEXT),
        ('ensemble-blocked BTC/USD conf=0.81', TEXT),
        ('  votes=[trend:buy@1.00x0.75,', MUTED),
        ('         breakout:buy@1.00x0.75,', MUTED),
        ('         xsmom:buy@1.00x1.00,', MUTED),
        ('         meanrev:hold@0.50x1.25]', MUTED),
        ('', TEXT),
        ('# every decision journaled with', MUTED),
        ('# the full vote trail', MUTED),
    ]
    y = 240
    for txt, color in code_lines:
        d.text((720, y), txt, font=F_CODE, fill=color)
        y += 26

    footer(d, "alpacaruns auto --dry-run replays the same pipeline without orders.")
    img.save(os.path.join(OUT_DIR, "slide_07.png"))

for fn in (slide1, slide2, slide3, slide4, slide5, slide6, slide7):
    fn()
    print("wrote", fn.__name__)
print("done")
