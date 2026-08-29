// CratePile's figure bound, its own module because react-refresh objects to a
// component file that also exports functions.
//
// Bounded at FOUR glyphs, which is what keeps the figure inside its box: at
// 27px mono each glyph is about 16px and FlapCount puts 3px between them, so
// four is 74px and five would not fit beside the pile at any sensible width.
//
// Precision drops as magnitude grows, the same way SpendRing's money rule
// works. Exact under a thousand, because a backlog of 340 is a number someone
// acts on; one decimal to ten thousand; whole thousands past that, where the
// last three digits of a backlog are noise.
export const compactCount = (n: number): string => {
  if (n < 1000) return String(n)
  if (n < 10000) return (Math.round(n / 100) / 10).toFixed(1) + 'k'
  return Math.round(n / 1000) + 'k'
}
