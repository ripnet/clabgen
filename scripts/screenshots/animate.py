"""Builds the tour animation from tour.mjs's frames:
    python3 animate.py FRAME_DIR OUT.webp
Frames keep their durations; scene changes crossfade. Lossless: UI
screens compress better that way than lossy, and stay sharp."""

import json
import sys
from pathlib import Path

from PIL import Image

FADE_STEPS = 4  # frames per crossfade
FADE_MS = 60  # each


def main(src, dst):
    src = Path(src)
    frames = json.loads((src / "frames.json").read_text())
    images, durations = [], []
    prev = None
    for f in frames:
        img = Image.open(src / f["file"]).convert("RGB")
        if prev is not None and prev["scene"] != f["scene"]:
            last = images[-1]
            for i in range(1, FADE_STEPS + 1):
                images.append(Image.blend(last, img, i / (FADE_STEPS + 1)))
                durations.append(FADE_MS)
        images.append(img)
        durations.append(f["ms"])
        prev = f
    images[0].save(dst, save_all=True, append_images=images[1:], duration=durations, loop=0, lossless=True, method=4)
    print(f"{dst}: {len(images)} frames, {sum(durations) / 1000:.1f} s, {Path(dst).stat().st_size / 1e6:.2f} MB")


if __name__ == "__main__":
    main(*sys.argv[1:])
