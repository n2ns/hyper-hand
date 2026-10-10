# Draws hyperhand.ico, the screen icon of hyperhand.exe, its tray icon and settings window (Pillow), and preview.png.
# Sizes 256 and 48 are drawn large and scaled down; 32, 24 and 16 are placed pixel by pixel.
# Regenerate the resources afterwards, in cmd/hyperhand and again in cmd/hyperhand-agent: go-winres make --in winres/winres.json --arch amd64

from PIL import Image, ImageDraw
W = (255, 255, 255, 255)
B = (0, 0, 0, 0)  # punched back to the background

def background(S, margin, radius):
    top, bot = (0x33, 0x80, 0xE6), (0x16, 0x52, 0xB0)
    col = Image.new("RGBA", (1, S))
    for y in range(S):
        t = y / max(S - 1, 1)
        col.putpixel((0, y), tuple(int(a + (b - a) * t) for a, b in zip(top, bot)) + (255,))
    bg = col.resize((S, S))
    mask = Image.new("L", (S, S), 0)
    ImageDraw.Draw(mask).rounded_rectangle((margin, margin, S - 1 - margin, S - 1 - margin), radius=radius, fill=255)
    img = Image.new("RGBA", (S, S), (0, 0, 0, 0))
    img.paste(bg, (0, 0), mask)
    return img

def big():
    S = 1024
    img = background(S, 24, 210)
    mask = Image.new("L", (S, S), 0)
    d = ImageDraw.Draw(mask)
    d.rounded_rectangle((90, 110, 934, 740), radius=70, fill=255)    # screen frame
    d.rounded_rectangle((160, 180, 864, 670), radius=35, fill=0)
    d.rectangle((454, 740, 570, 830), fill=255)                      # stand
    d.rounded_rectangle((300, 820, 724, 890), radius=35, fill=255)
    img.paste(Image.new("RGBA", (S, S), W), (0, 0), mask)
    return img

def pixel(n):
    # white rectangles (x0, y0, x1, y1), inclusive: frame, inside punched out, neck, base
    rects = {16: [(1, 2, 14, 11, W), (2, 3, 13, 10, B), (7, 12, 8, 12, W), (4, 13, 11, 13, W)],
             24: [(2, 2, 21, 16, W), (4, 4, 19, 14, B), (11, 17, 12, 18, W), (6, 19, 17, 20, W)],
             32: [(2, 3, 29, 22, W), (4, 5, 27, 20, B), (15, 23, 16, 25, W), (9, 26, 22, 27, W)]}[n]
    img = background(n, 0, {16: 3, 24: 4, 32: 6}[n])
    bg = img.copy()
    for x0, y0, x1, y1, c in rects:
        for y in range(y0, y1 + 1):
            for x in range(x0, x1 + 1):
                if bg.getpixel((x, y))[3]:  # leave the rounded corners clear
                    img.putpixel((x, y), bg.getpixel((x, y)) if c == B else c)
    return img

order = [256, 48, 32, 24, 16]
b = big()
imgs = {256: b.resize((256, 256), Image.LANCZOS), 48: b.resize((48, 48), Image.BOX),
        32: pixel(32), 24: pixel(24), 16: pixel(16)}
imgs[256].save("hyperhand.ico", sizes=[(s, s) for s in order], append_images=[imgs[s] for s in order[1:]])
sheet = Image.new("RGBA", (1180, 620), (255, 255, 255, 255))
ImageDraw.Draw(sheet).rectangle((0, 310, 1180, 620), fill=(32, 32, 32, 255))
for yo in (10, 320):
    x = 10
    for s in order:
        z = imgs[s].resize((200, 200), Image.NEAREST if s < 256 else Image.LANCZOS)
        sheet.alpha_composite(z, (x, yo)); x += 210
    x = 10
    for s in order[1:]:
        sheet.alpha_composite(imgs[s], (x, yo + 230)); x += 70
sheet.save("preview.png")
print("ok")
