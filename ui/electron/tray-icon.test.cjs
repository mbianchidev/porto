const assert = require('node:assert/strict')
const test = require('node:test')
const zlib = require('node:zlib')

const { createTrayIcon, trayGlyphPixels } = require('./tray-icon.cjs')

function fakeNativeImage() {
  const image = {
    representations: [],
    template: undefined,
    addRepresentation(options) { this.representations.push(options) },
    isEmpty() { return this.representations.length === 0 },
    setTemplateImage(value) { this.template = value },
  }
  return { image, nativeImage: { createEmpty: () => image } }
}

function decodePNG(buffer) {
  assert.equal(buffer.subarray(1, 4).toString('ascii'), 'PNG')
  let offset = 8
  let width = 0
  const data = []
  while (offset < buffer.length) {
    const length = buffer.readUInt32BE(offset)
    const type = buffer.toString('ascii', offset + 4, offset + 8)
    const body = buffer.subarray(offset + 8, offset + 8 + length)
    assert.equal(buffer.readUInt32BE(offset + 8 + length), zlib.crc32(body, zlib.crc32(type)))
    if (type === 'IHDR') width = body.readUInt32BE(0)
    if (type === 'IDAT') data.push(body)
    offset += length + 12
  }
  const scanlines = zlib.inflateSync(Buffer.concat(data))
  const rgba = Buffer.alloc(width * width * 4)
  for (let row = 0; row < width; row += 1) {
    assert.equal(scanlines[row * (width * 4 + 1)], 0)
    scanlines.copy(rgba, row * width * 4, row * (width * 4 + 1) + 1, (row + 1) * (width * 4 + 1))
  }
  return { width, rgba }
}

function colors(rgba) {
  const seen = new Set()
  for (let index = 0; index < rgba.length; index += 4) {
    if (rgba[index + 3] > 0) seen.add(rgba.subarray(index, index + 3).join(','))
  }
  return [...seen]
}

test('macOS tray icon is a monochrome template image at 1x and 2x', () => {
  const { image, nativeImage } = fakeNativeImage()
  assert.equal(createTrayIcon(nativeImage, { shouldUseDarkColors: false }, 'darwin'), image)
  assert.equal(image.template, true)
  assert.deepEqual(image.representations.map(({ scaleFactor }) => scaleFactor), [1, 2])
  for (const { scaleFactor, buffer } of image.representations) {
    const { width, rgba } = decodePNG(buffer)
    assert.equal(width, 18 * scaleFactor)
    assert.deepEqual(colors(rgba), ['0,0,0'])
  }
})

test('Windows and Linux tray icons follow the system theme', () => {
  const cases = [
    ['win32', { shouldUseDarkColors: false, shouldUseDarkColorsForSystemIntegratedUI: true }, '255,255,255'],
    ['win32', { shouldUseDarkColors: true, shouldUseDarkColorsForSystemIntegratedUI: false }, '0,0,0'],
    ['linux', { shouldUseDarkColors: true }, '255,255,255'],
    ['linux', { shouldUseDarkColors: false }, '0,0,0'],
  ]
  for (const [platform, theme, color] of cases) {
    const { image, nativeImage } = fakeNativeImage()
    createTrayIcon(nativeImage, theme, platform)
    assert.equal(image.template, false)
    for (const { scaleFactor, buffer } of image.representations) {
      const { width, rgba } = decodePNG(buffer)
      assert.equal(width, 20 * scaleFactor)
      assert.deepEqual(colors(rgba), [color], `${platform} ${JSON.stringify(theme)}`)
    }
  }
})

test('tray glyph keeps the three lights separate inside an open frame', () => {
  const pixels = 36
  const rgba = trayGlyphPixels(pixels, [0, 0, 0])
  const alpha = (x, y) => rgba[(Math.floor(y * 2) * pixels + Math.floor(x * 2)) * 4 + 3]
  for (const light of [5.3, 9, 12.7]) assert.equal(alpha(light, 9), 255)
  for (const gap of [7.15, 10.85]) assert.equal(alpha(gap, 9), 0)
  assert.equal(alpha(2.25, 9), 255)
  assert.equal(alpha(9, 4.5), 0)
  assert.equal(alpha(0.5, 0.5), 0)
})
