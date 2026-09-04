#!/usr/bin/env python
"""Build the v2 slide show from the live dashboard screenshots.

Source: hackathon_video/screenshots_v2/{welcome,live,trades,brain,controls}.png
Output: hackathon_video/slides_v2/slide_NN.png + assembled video

Each slide is a 1280x720 frame with:
  - the dashboard screenshot centered (with a small padding + caption)
  - a title strip at the top (from the captions dict)
  - a footer strip with the slide number + branding

The script also bundles the per-slide PNGs into a final MP4 (using ffmpeg
via subprocess). The audio tracks from slide_audio/ are muxed in.
"""
from __future__ import annotations

import os
import subprocess
from pathlib import Path

from PIL import Image, ImageDraw, ImageFont

ROOT = Path(__file__).resolve().parent.parent
SHOTS = ROOT / "hackathon_video" / "screenshots_v2"
OUT = ROOT / "hackathon_video" / "slides_v2"
AUDIO = ROOT / "hackathon_video" / "slide_audio_v2"
OUT.mkdir(parents=True, exist_ok=True)
AUDIO.mkdir(parents=True, exist_ok=True)

W, H = 1280, 720
BG_TOP = (10, 14, 23)
BG_BOT = (16, 22, 36)
ACCENT = (16, 185, 129)   # emerald-500
TEXT = (231, 236, 247)
MUTED = (156, 163, 175)
DIVIDER = (55, 65, 90)

# Each slide = a screenshot + a caption + a per-slide audio file.
SLIDES = [
    {
        "shot": "welcome.png",
        "title": "Alpacaruns — Multi-Agent Paper Trading",
        "subtitle": "Live demo console · http://5.22.215.51",
        "audio": "slide_01.mp3",
    },
    {
        "shot": "live.png",
        "title": "Live — Status, P&L, Equity Curve",
        "subtitle": "Auto-refresh every 5s · drawdown halts visible",
        "audio": "slide_02.mp3",
    },
    {
        "shot": "trades.png",
        "title": "Trades — Explainability",
        "subtitle": "Path, confidence, factor scores, risk checks",
        "audio": "slide_03.mp3",
    },
    {
        "shot": "brain.png",
        "title": "Brain — Agent + Ensemble Decisions",
        "subtitle": "Open positions · recent decision feed",
        "audio": "slide_04.mp3",
    },
    {
        "shot": "controls.png",
        "title": "Controls — Safe Actions",
        "subtitle": "Pause / Resume / Step · read-only risk config",
        "audio": "slide_05.mp3",
    },
]

SLIDE_DURATION = 7.0  # seconds per slide (matches audio length)


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


def letterbox(img: Image.Image, max_w: int, max_h: int) -> Image.Image:
    """Resize img to fit inside (max_w, max_h) keeping aspect ratio."""
    iw, ih = img.size
    s = min(max_w / iw, max_h / ih)
    nw, nh = max(1, int(iw * s)), max(1, int(ih * s))
    return img.resize((nw, nh), Image.LANCZOS)


def draw_gradient_bg() -> Image.Image:
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


def make_slide(idx: int, total: int, spec: dict) -> Image.Image:
    bg = draw_gradient_bg()
    d = ImageDraw.Draw(bg)

    # Title bar
    d.rectangle([(0, 0), (W, 90)], fill=(8, 12, 20))
    d.text((40, 18), "ALPACARUNS · Demo", font=F_TAG, fill=ACCENT)
    d.text((W - 140, 18), f"Slide {idx} / {total}", font=F_NUM, fill=MUTED)
    d.rectangle([(0, 90), (W, 92)], fill=ACCENT)

    # Title + subtitle
    d.text((40, 110), spec["title"], font=F_TITLE, fill=TEXT)
    d.text((40, 158), spec["subtitle"], font=F_SUB, fill=MUTED)

    # Screenshot area
    shot = Image.open(SHOTS / spec["shot"]).convert("RGB")
    target_w, target_h = W - 80, H - 240
    fit = letterbox(shot, target_w, target_h)
    x = (W - fit.size[0]) // 2
    y = 200 + (target_h - fit.size[1]) // 2
    # 1px accent border around the screenshot
    d.rectangle(
        [(x - 2, y - 2), (x + fit.size[0] + 1, y + fit.size[1] + 1)],
        outline=DIVIDER,
        width=2,
    )
    bg.paste(fit, (x, y))

    # Footer
    d.rectangle([(0, H - 50), (W, H)], fill=(8, 12, 20))
    d.text(
        (40, H - 36),
        "Live bot · 24/7 on UpCloud otemaach · paper trading only",
        font=F_TAG,
        fill=MUTED,
    )

    return bg


def main() -> None:
    total = len(SLIDES)
    for i, spec in enumerate(SLIDES, 1):
        img = make_slide(i, total, spec)
        out = OUT / f"slide_{i:02d}.png"
        img.save(out, optimize=True)
        print(f"  wrote {out.relative_to(ROOT)} ({out.stat().st_size//1024} KB)")

    # Build per-slide MP4s (static image + audio) using ffmpeg.
    # The video length = audio length + 1s tail so each slide has a
    # beat after the narration ends.
    per_slide = ROOT / "hackathon_video" / "per_slide_v2"
    per_slide.mkdir(parents=True, exist_ok=True)

    def audio_duration(p: Path) -> float:
        r = subprocess.run(
            [
                "ffprobe", "-v", "error",
                "-show_entries", "format=duration",
                "-of", "default=noprint_wrappers=1:nokey=1",
                str(p),
            ],
            capture_output=True, text=True, check=True,
        )
        return float(r.stdout.strip())

    total_runtime = 0.0
    for i, spec in enumerate(SLIDES, 1):
        png = OUT / f"slide_{i:02d}.png"
        audio = AUDIO / spec["audio"]
        if not audio.exists():
            raise SystemExit(f"missing audio: {audio} (run tools/build_demo_narration.py)")
        dur = audio_duration(audio) + 1.0
        total_runtime += dur

        out_mp4 = per_slide / f"slide_{i:02d}.mp4"
        subprocess.run(
            [
                "ffmpeg",
                "-y",
                "-loop", "1",
                "-i", str(png),
                "-i", str(audio),
                "-c:v", "libx264",
                "-tune", "stillimage",
                "-c:a", "aac",
                "-b:a", "192k",
                "-pix_fmt", "yuv420p",
                "-t", f"{dur:.2f}",
                "-vf", "scale=1280:720",
                str(out_mp4),
            ],
            check=True,
            capture_output=True,
        )
        print(f"  wrote {out_mp4.relative_to(ROOT)} ({out_mp4.stat().st_size//1024} KB, {dur:.1f}s)")

    # Concatenate per-slide videos into the final showcase
    concat_txt = per_slide / "concat.txt"
    with concat_txt.open("w") as f:
        for i in range(1, total + 1):
            f.write(f"file 'slide_{i:02d}.mp4'\n")
    final_mp4 = ROOT / "hackathon_video" / "alpacaruns_demo_v2.mp4"
    subprocess.run(
        [
            "ffmpeg",
            "-y",
            "-f", "concat",
            "-safe", "0",
            "-i", str(concat_txt),
            "-c", "copy",
            str(final_mp4),
        ],
        check=True,
        capture_output=True,
    )
    print(f"\nFinal video: {final_mp4.relative_to(ROOT)} ({final_mp4.stat().st_size//1024} KB)")
    print(f"  {total} slides, total runtime ~{total_runtime:.1f}s")


if __name__ == "__main__":
    main()
