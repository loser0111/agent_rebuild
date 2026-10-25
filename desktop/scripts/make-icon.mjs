#!/usr/bin/env node
/**
 * 生成应用图标（无第三方依赖）。
 *
 * 产出 build/icon.png（1024x1024）：深色圆角方块 + 绿色菱形标记，
 * 与界面品牌标识一致。electron-builder 会据此自动生成各平台图标格式。
 *
 * 用法：node scripts/make-icon.mjs
 */
import { deflateSync } from 'node:zlib'
import { mkdir, writeFile } from 'node:fs/promises'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const here = path.dirname(fileURLToPath(import.meta.url))
const outDir = path.resolve(here, '..', 'build')

const SIZE = 1024

/** 生成 RGBA 像素并按 PNG 规范编码（手写，无依赖）。 */
function makePNG(size) {
  const bg = [0x14, 0x14, 0x14, 0xff] // 近黑背景
  const accent = [0x10, 0xa3, 0x7f, 0xff] // 品牌绿
  const corner = size * 0.22 // 圆角半径
  const cx = size / 2
  const cy = size / 2
  const d = size * 0.3 // 菱形半对角

  // 每行前加 filter byte(0)，再拼上 RGBA
  const raw = Buffer.alloc((size * 4 + 1) * size)
  let p = 0
  for (let y = 0; y < size; y++) {
    raw[p++] = 0 // filter: None
    for (let x = 0; x < size; x++) {
      // 圆角矩形遮罩
      const inRounded = isInRoundedRect(x, y, size, corner)
      if (!inRounded) {
        raw[p++] = 0
        raw[p++] = 0
        raw[p++] = 0
        raw[p++] = 0
        continue
      }
      // 菱形品牌标记：|dx|+|dy| <= d
      const insideDiamond = Math.abs(x - cx) + Math.abs(y - cy) <= d
      const color = insideDiamond ? accent : bg
      raw[p++] = color[0]
      raw[p++] = color[1]
      raw[p++] = color[2]
      raw[p++] = color[3]
    }
  }

  const ihdr = Buffer.alloc(13)
  ihdr.writeUInt32BE(size, 0)
  ihdr.writeUInt32BE(size, 4)
  ihdr[8] = 8 // bit depth
  ihdr[9] = 6 // color type RGBA
  ihdr[10] = 0
  ihdr[11] = 0
  ihdr[12] = 0

  return Buffer.concat([
    Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
    chunk('IHDR', ihdr),
    chunk('IDAT', deflateSync(raw, { level: 9 })),
    chunk('IEND', Buffer.alloc(0)),
  ])
}

function isInRoundedRect(x, y, size, r) {
  const inX = x >= r && x <= size - r
  const inY = y >= r && y <= size - r
  if (inX || inY) {
    // 边带区域：只要在矩形内即可
    if (inX && y >= 0 && y < size) return true
    if (inY && x >= 0 && x < size) return true
  }
  // 四个圆角
  const corners = [
    [r, r],
    [size - r, r],
    [r, size - r],
    [size - r, size - r],
  ]
  for (const [ccx, ccy] of corners) {
    if (Math.hypot(x - ccx, y - ccy) <= r) return true
  }
  return false
}

function chunk(type, data) {
  const len = Buffer.alloc(4)
  len.writeUInt32BE(data.length, 0)
  const typeBuf = Buffer.from(type, 'ascii')
  const crcBuf = Buffer.alloc(4)
  crcBuf.writeUInt32BE(crc32(Buffer.concat([typeBuf, data])) >>> 0, 0)
  return Buffer.concat([len, typeBuf, data, crcBuf])
}

const CRC_TABLE = (() => {
  const table = new Int32Array(256)
  for (let n = 0; n < 256; n++) {
    let c = n
    for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1
    table[n] = c
  }
  return table
})()

function crc32(buf) {
  let c = 0xffffffff
  for (let i = 0; i < buf.length; i++) c = CRC_TABLE[(c ^ buf[i]) & 0xff] ^ (c >>> 8)
  return c ^ 0xffffffff
}

await mkdir(outDir, { recursive: true })
const png = makePNG(SIZE)
await writeFile(path.join(outDir, 'icon.png'), png)
console.log(`✅ 图标已生成: build/icon.png (${SIZE}x${SIZE}, ${(png.length / 1024).toFixed(1)}KB)`)
