#!/bin/sh
# Makes assets/tour.gif, the animation the README shows: the app walks
# through its screens with demo data, drawing each frame itself (no screen
# is captured), and ffmpeg makes a GIF of the frames with a palette of its
# own. Run from the repository's root; needs Go and ffmpeg.
set -eu

frames=$(mktemp -d)
trap 'rm -rf "$frames"' EXIT

go build -o "$frames/gvx-desktop" .
"$frames/gvx-desktop" -data-dir "$frames/data" -render-tour "$frames/png"

# 15 frames a second are drawn; 10 are kept, at three quarters the size.
ffmpeg -y -loglevel error -framerate 15 -i "$frames/png/%04d.png" \
	-vf "fps=10,scale=960:-1:flags=lanczos,split[a][b];[a]palettegen=max_colors=128:stats_mode=diff[p];[b][p]paletteuse=dither=bayer:bayer_scale=5:diff_mode=rectangle" \
	-loop 0 assets/tour.gif
ls -lh assets/tour.gif
