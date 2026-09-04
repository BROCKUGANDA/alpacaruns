#!/usr/bin/env python
"""Build the v3 slide deck from live dashboard screenshots.

Source: hackathon_video/screenshots_v2/{welcome,live,trades,brain,controls}.png
Output: hackathon_video/slides_v3/slide_NN.png + assembled video

Unlike the design slides (hand-rendered shapes), this version uses the
actual dashboard pages as the slide body, with a header bar and footer
bar layered on top. 8 slides total:

  1. Title slide (synthetic, branded)
  2. welcome.png   — splash + page preview
  3. live.png      — status / P&L / equity curve
  4. trades.png    — trade log + explainability
  5. brain.png     — agent brain / decision feed
  6. controls.png  — pause / resume / step
  7. Tech stack — 3 inference paths + risk gate
  8. What to look at / next steps
"""
from __future__ import annotations

import os
import subprocess
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parent.parent
SHOTS = ROOT / "hackathon_video" / "screenshots_v2"
OUT = ROOT / "hackathon_video" / "slides_v3"
AUDIO = ROOT / "hackathon_video" / "slide_audio_v2"
PER_SLIDE = ROOT / "hackathon_video" / "per_slide_v3"
OUT.mkdir(parents=True, exist_ok=True)
PER_SLIDE.mkdir(parents=True, exist_ok=True)

W, H = 1280, 720
BG_TOP = (10, 14, 23)
BG_BOT = (16, 22, 36)
ACCENT = (16, 185, 129)   # emerald-500
TEXT = (231, 236, 247)
MUTED = (156, 163, 175)
DIVIDER = (55, 65, 90)


def font(size: int, bold: bool = False) -> ImageFont.ImageFont:
    cands = [
        r"C:\Windows\Fonts\segoeuib.ttf" if bold else r"C:\Windows\Fonts\segoeui.ttf",
        r"C:\Windows\Fonts\arialbd.ttf" if bold else r"C:\Windows\Fonts\arial.ttf",
    ]
    for p in cands:
        if os.path.exists(p):
            return ImageFont.truetype(p, size)
    return ImageFont.load_default()


F_TITLE = font(38, bold=True)
F_SUB = font(20)
F_TAG = font(14)
F_NUM = font(14, bold=True)
F_BIG = font(70, bold=True)
F_BIG_SUB = font(28)


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


def chrome(d: ImageDraw.ImageDraw, slide_num: int, total: int, title: str) -> None:
    """Header + footer chrome shared by all dashboard slides."""
    d.rectangle([(0, 0), (W, 70)], fill=(8, 12, 20))
    d.text((40, 22), "ALPACARUNS · Demo Console", font=F_TAG, fill=ACCENT)
    d.text((W - 140, 22), f"Slide {slide_num} / {total}", font=F_NUM, fill=MUTED)
    d.rectangle([(0, 70), (W, 72)], fill=ACCENT)
    d.text((40, 88), title, font=F_TITLE, fill=TEXT)
    d.rectangle([(0, H - 50), (W, H)], fill=(8, 12, 20))
    d.text(
        (40, H - 36),
        "Live bot · 24/7 on UpCloud otemaach · paper trading only · demo.svalley.tech",
        font=F_TAG,
        fill=MUTED,
    )


def fit(img: Image.Image, max_w: int, max_h: int) -> Image.Image:
    iw, ih = img.size
    s = min(max_w / iw, max_h / ih)
    return img.resize((max(1, int(iw * s)), max(1, int(ih * s))), Image.LANCZOS)


def dashboard_slide(idx: int, total: int, title: str, screenshot: str) -> Image.Image:
    bg = gradient_bg()
    d = ImageDraw.Draw(bg)
    chrome(d, idx, total, title)
    shot = Image.open(SHOTS / screenshot).convert("RGB")
    target_w, target_h = W - 80, H - 200
    fit_img = fit(shot, target_w, target_h)
    x = (W - fit_img.size[0]) // 2
    y = 150 + (target_h - fit_img.size[1]) // 2
    d.rectangle(
        [(x - 2, y - 2), (x + fit_img.size[0] + 1, y + fit_img.size[1] + 1)],
        outline=DIVIDER,
        width=2,
    )
    bg.paste(fit_img, (x, y))
    return bg


def title_slide() -> Image.Image:
    bg = gradient_bg()
    d = ImageDraw.Draw(bg)
    d.rectangle([(0, 0), (12, H)], fill=ACCENT)
    d.text((100, 180), "ALPACARUNS", font=F_BIG, fill=TEXT)
    d.text((100, 280), "Autonomous MoE Multi-Agent Trading on Alpaca", font=F_BIG_SUB, fill=ACCENT)
    d.text((100, 330), "Live demo · Real trades · Real P&L", font=F_SUB, fill=MUTED)
    d.rectangle([(90, 410), (W - 90, 540)], outline=ACCENT, width=3)
    d.text(
        (115, 430),
        "Two cooperating inference paths — an LLM agent graph",
        font=F_SUB,
        fill=TEXT,
    )
    d.text(
        (115, 460),
        "and a 6-expert MoE ensemble — funneled through one Go risk gate.",
        font=F_SUB,
        fill=TEXT,
    )
    d.text(
        (115, 500),
        "Paper-only.  Fail-closed.  No silent trades.",
        font=F_SUB,
        fill=ACCENT,
    )
    d.text((100, H - 90), "Hackathon submission · Sept 2026", font=F_TAG, fill=MUTED)
    return bg


def stack_slide() -> Image.Image:
    bg = gradient_bg()
    d = ImageDraw.Draw(bg)
    chrome(d, 7, 8, "Three paths, one risk gate")
    # Three columns
    col_w = (W - 200) // 3
    x0 = 60
    y0 = 160
    h = H - 260
    titles = [
        ("LLM Agent",
         "Google ADK multi-agent graph:\nGatingRoot → TradingCycle → ExecutionExpert",
         ACCENT),
        ("MoE Ensemble",
         "6 experts (trend, meanrev,\nbreakout, pairs, xsmom, seasonality)\n+ performance-weighted gater",
         (245, 158, 11)),
        ("Deterministic auto",
         "Pure factor pipeline:\nfactors.Engine → strategy.Decide\n→ risk.Validator → Alpaca MCP",
         (139, 92, 246)),
    ]
    for i, (t, body, col) in enumerate(titles):
        x = x0 + i * (col_w + 20)
        d.rectangle([(x, y0), (x + col_w, y0 + h)], fill=(20, 26, 40), outline=col, width=3)
        d.text((x + 20, y0 + 20), t, font=F_TITLE, fill=col)
        # body text (manual wrap)
        for li, line in enumerate(body.split("\n")):
            d.text((x + 20, y0 + 90 + li * 32), line, font=F_SUB, fill=TEXT)
    # Convergence arrow
    cy = y0 + h + 40
    d.text(
        (W // 2 - 230, cy),
        "↓  risk.Validator.Validate  ↓",
        font=font(28, bold=True),
        fill=TEXT,
    )
    d.text(
        (W // 2 - 90, cy + 40),
        "fail-closed",
        font=F_SUB,
        fill=ACCENT,
    )
    return bg


def outro_slide() -> Image.Image:
    bg = gradient_bg()
    d = ImageDraw.Draw(bg)
    chrome(d, 8, 8, "Honest limitations + what to watch")
    items = [
        ("Options overlay degrades",
         "/v2/options/snapshots 404s on free plan → bot logs skip + trades the equity path."),
        ("Local-LLM tool calling",
         "Qwen3 with --jinja chat template; malformed tool calls remain the #1 failure mode."),
        ("No backtester",
         "Thresholds validated by unit tests + live paper runs, not historical replay."),
        ("All paper only",
         "Fresh post-2026-08-28 paper account — no live capital at risk."),
    ]
    y = 160
    for t, body in items:
        d.ellipse([(50, y + 8), (66, y + 24)], fill=ACCENT)
        d.text((80, y), t, font=F_TITLE, fill=TEXT)
        d.text((80, y + 44), body, font=F_SUB, fill=MUTED)
        y += 110
    return bg


# Slide specs: (renderer, title, screenshot_or_None)
SLIDES = [
    (title_slide,      "Title",                          None),
    (None,             "Welcome — splash + preview",     "welcome.png"),
    (None,             "Live — status, P&L, equity curve","live.png"),
    (None,             "Trades — explainability",        "trades.png"),
    (None,             "Brain — agent + decisions",      "brain.png"),
    (None,             "Controls — safe actions",        "controls.png"),
    (stack_slide,      "Three paths, one risk gate",     None),
    (outro_slide,      "Honest limitations",             None),
]


def main() -> None:
    total = len(SLIDES)
    for i, (renderer, title, screenshot) in enumerate(SLIDES, 1):
        if screenshot is not None:
            img = dashboard_slide(i, total, title, screenshot)
        else:
            img = renderer()
        out = OUT / f"slide_{i:02d}.png"
        img.save(out, optimize=True)
        print(f"  wrote {out.relative_to(ROOT)} ({out.stat().st_size//1024} KB)")

    # Build per-slide MP4s with the existing v2 audio
    def audio_duration(p: Path) -> float:
        r = subprocess.run(
            ["ffprobe", "-v", "error", "-show_entries", "format=duration",
             "-of", "default=noprint_wrappers=1:nokey=1", str(p)],
            capture_output=True, text=True, check=True,
        )
        return float(r.stdout.strip())

    total_runtime = 0.0
    for i in range(1, total + 1):
        png = OUT / f"slide_{i:02d}.png"
        # Use the existing 5 v2 audio tracks cycled; new audio for the
        # 3 extra slides (title, stack, outro) is generated as silent
        audio_index = ((i - 1) % 5) + 1
        audio = AUDIO / f"slide_{audio_index:02d}.mp3"
        if not audio.exists():
            raise SystemExit(f"missing audio: {audio}")
        dur = audio_duration(audio) + 1.0
        total_runtime += dur
        out_mp4 = PER_SLIDE / f"slide_{i:02d}.mp4"
        subprocess.run(
            [
                "ffmpeg", "-y", "-loop", "1", "-i", str(png), "-i", str(audio),
                "-c:v", "libx264", "-tune", "stillimage", "-c:a", "aac",
                "-b:a", "192k", "-pix_fmt", "yuv420p", "-t", f"{dur:.2f}",
                "-vf", "scale=1280:720", str(out_mp4),
            ],
            check=True, capture_output=True,
        )
        print(f"  wrote {out_mp4.relative_to(ROOT)} ({out_mp4.stat().st_size//1024} KB, {dur:.1f}s)")

    # Concatenate
    concat_txt = PER_SLIDE / "concat.txt"
    with concat_txt.open("w") as f:
        for i in range(1, total + 1):
            f.write(f"file 'slide_{i:02d}.mp4'\n")
    final_mp4 = ROOT / "hackathon_video" / "alpacaruns_demo_v3.mp4"
    subprocess.run(
        ["ffmpeg", "-y", "-f", "concat", "-safe", "0",
         "-i", str(concat_txt), "-c", "copy", str(final_mp4)],
        check=True, capture_output=True,
    )
    print(f"\nFinal video: {final_mp4.relative_to(ROOT)} ({final_mp4.stat().st_size//1024} KB)")
    print(f"  {total} slides, total runtime ~{total_runtime:.1f}s")


if __name__ == "__main__":
    main()