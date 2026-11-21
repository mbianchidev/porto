// Monochrome tray glyph: the app icon's rounded tile drawn as an outline around
// its three lights. It is rendered in code so every scale factor stays crisp.

const zlib = require('node:zlib')

const DESIGN_SIZE = 18
const SCALE_FACTORS = [1, 2]
const FRAME = { half: 6.75, radius: 2.85, stroke: 1.5 }
const LIGHT_RADIUS = 1.45
const LIGHT_CENTERS = [5.3, 9, 12.7]
const BLACK = [0, 0, 0]
const WHITE = [255, 255, 255]
const PNG_SIGNATURE = Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a])

// Signed distance in design units from (x, y) to the glyph edge; negative inside.
function glyphDistance(x, y) {
  const dx = Math.abs(x - DESIGN_SIZE / 2) - (FRAME.half - FRAME.radius)
  const dy = Math.abs(y - DESIGN_SIZE / 2) - (FRAME.half - FRAME.radius)
  const outside = Math.hypot(Math.max(dx, 0), Math.max(dy, 0))
  const frame = Math.abs(outside + Math.min(Math.max(dx, dy), 0) - FRAME.radius) - FRAME.stroke / 2
  const lights = Math.min(...LIGHT_CENTERS.map((cx) => Math.hypot(x - cx, y - DESIGN_SIZE / 2) - LIGHT_RADIUS))
  return Math.min(frame, lights)
}

function trayGlyphPixels(pixels, [red, green, blue]) {
  const unit = DESIGN_SIZE / pixels
  const rgba = Buffer.alloc(pixels * pixels * 4)
  for (let row = 0; row < pixels; row += 1) {
    for (let column = 0; column < pixels; column += 1) {
      const distance = glyphDistance((column + 0.5) * unit, (row + 0.5) * unit) / unit
      const alpha = Math.round(255 * Math.min(1, Math.max(0, 0.5 - distance)))
      rgba.set([red, green, blue, alpha], (row * pixels + column) * 4)
    }
  }
  return rgba
}

function pngChunk(type, data) {
  const header = Buffer.alloc(8)
  header.writeUInt32BE(data.length, 0)
  header.write(type, 4, 'ascii')
  const crc = Buffer.alloc(4)
  crc.writeUInt32BE(zlib.crc32(data, zlib.crc32(header.subarray(4))), 0)
  return Buffer.concat([header, data, crc])
}

function encodePNG(pixels, rgba) {
  const stride = pixels * 4
  const scanlines = Buffer.alloc((stride + 1) * pixels)
  for (let row = 0; row < pixels; row += 1) {
    rgba.copy(scanlines, row * (stride + 1) + 1, row * stride, (row + 1) * stride)
  }
  const header = Buffer.alloc(13)
  header.writeUInt32BE(pixels, 0)
  header.writeUInt32BE(pixels, 4)
  header.set([8, 6, 0, 0, 0], 8)
  return Buffer.concat([
    PNG_SIGNATURE,
    pngChunk('IHDR', header),
    pngChunk('IDAT', zlib.deflateSync(scanlines)),
    pngChunk('IEND', Buffer.alloc(0)),
  ])
}

// macOS template images are tinted by the menu bar, so only their alpha matters.
// Windows reports the taskbar theme separately from the app theme.
function trayIconAppearance(platform, theme) {
  if (platform === 'darwin') return { size: DESIGN_SIZE, template: true, color: BLACK }
  const dark = platform === 'win32' ? theme.shouldUseDarkColorsForSystemIntegratedUI : theme.shouldUseDarkColors
  return { size: 20, template: false, color: dark ? WHITE : BLACK }
}

function createTrayIcon(nativeImage, theme, platform = process.platform) {
  const { size, template, color } = trayIconAppearance(platform, theme)
  const image = nativeImage.createEmpty()
  for (const scaleFactor of SCALE_FACTORS) {
    const pixels = Math.round(size * scaleFactor)
    image.addRepresentation({ scaleFactor, buffer: encodePNG(pixels, trayGlyphPixels(pixels, color)) })
  }
  if (image.isEmpty()) throw new Error('Unable to render the Porto tray icon')
  image.setTemplateImage(template)
  return image
}

module.exports = { createTrayIcon, trayGlyphPixels, trayIconAppearance }
