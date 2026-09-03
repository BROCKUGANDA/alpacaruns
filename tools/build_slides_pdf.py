#!/usr/bin/env python
"""Bundle the 7 hackathon slide PNGs into one PDF.

Source: hackathon_video/slides/slide_01.png .. slide_07.png
Output: docs/HACKATHON_SLIDES.pdf

Each slide is letterboxed onto an A4-landscape page so the PDF
renders consistently regardless of viewer DPI.
"""

from pathlib import Path
from PIL import Image

ROOT = Path(__file__).resolve().parent.parent
SRC = ROOT / "hackathon_video" / "slides"
DST = ROOT / "docs" / "HACKATHON_SLIDES.pdf"

# A4 landscape in points; PNGs are 1280x720 (16:9).
PAGE_SIZE = (842, 595)  # pts (A4 landscape)


def main() -> None:
    DST.parent.mkdir(parents=True, exist_ok=True)
    slides = sorted(SRC.glob("slide_*.png"))
    if not slides:
        raise SystemExit(f"no slide_*.png found under {SRC}")

    images: list[Image.Image] = []
    for slide_path in slides:
        img = Image.open(slide_path).convert("RGB")
        # Letterbox onto A4-landscape; preserves aspect ratio.
        page = Image.new("RGB", PAGE_SIZE, (10, 14, 23))  # dark bg #0a0e17
        iw, ih = img.size
        pw, ph = PAGE_SIZE
        scale = min(pw / iw, ph / ih)
        new_size = (max(1, int(iw * scale)), max(1, int(ih * scale)))
        img_resized = img.resize(new_size, Image.LANCZOS)
        x = (pw - new_size[0]) // 2
        y = (ph - new_size[1]) // 2
        page.paste(img_resized, (x, y))
        images.append(page)

    first, rest = images[0], images[1:]
    first.save(
        DST,
        save_all=True,
        append_images=rest,
        resolution=150,
        quality=92,
        optimize=True,
    )
    size_kb = DST.stat().st_size / 1024
    print(f"wrote {DST.relative_to(ROOT)} ({size_kb:.1f} KB, {len(images)} pages)")


if __name__ == "__main__":
    main()