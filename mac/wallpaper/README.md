# The Mac Studio's wallpaper

`mac-studio-dark.png` is the Studio's wallpaper, set by the `wallpaper` launchd
agent in `mac/hosts/mac-studio.nix`. It is 6016 x 3384, so it is sharp on a 6K
display and scales down to anything smaller.

It uses the kylejs.me design system: the dark theme's colour tokens, Lexend and
JetBrains Mono, a Mac Studio drawn on the same 24 grid as the site's marks,
and the site's ten area marks in a row under the name.

## Remaking it

The scripts read the marks and fonts from a kylejs-site checkout with its
dependencies installed (`pnpm install`), so a change to the site's tokens or
marks needs a remake, not an edit here. From this folder:

```sh
python3 -m pip install fonttools brotli numpy pillow
python3 make.py ~/code/kylejs-site .   # foreground SVGs, dark and light
node render.mjs ~/code/kylejs-site     # SVG to PNG, with the site's sharp
python3 compose.py dark                # background, dithered, plus foreground
```

`compose.py` draws the background in numpy rather than the SVG because the
glow behind the Studio spans only a few 8-bit colour steps, and an SVG render
shows them as rings. An ordered (Bayer) dither hides them and keeps the PNG
small. `python3 compose.py light` makes a light version too.

The colours in `make.py` and `compose.py` are copies of `src/design/tokens.ts`
in kylejs-site; update them together.
