// CratePile's figure bound, its own module because react-refresh objects to a
// component file that also exports functions.
//
// Bounded at FOUR glyphs, which is what keeps the figure inside its box: at
// 27px mono each glyph is about 16px and FlapCount puts 3px between them, so
// four is 74px and five would not fit beside the pile at any sensible width.
//
// Precision drops as magnitude grows, the same way SpendRing's money rule
// works. Exact under a thousand, because a backlog of 340 is a number someone
// acts on; one decimal under ten of a unit; whole units past that, where the
// trailing digits are noise. Each branch is chosen by the ROUNDED value, not
// the raw one — 9950 rounds to "10.0k", five glyphs, so a value that rounds
// out of its own precision band takes the next band's format instead.
export const compactCount = (n: number): string => {
  if (n < 1000) return String(n)
  let v = n
  for (const unit of ['k', 'm', 'b', 't']) {
    v /= 1000
    const tenths = Math.round(v * 10) / 10
    if (tenths < 10) return tenths.toFixed(1) + unit
    const whole = Math.round(v)
    if (whole < 1000) return String(whole) + unit
  }
  return Math.round(v) + 't'
}
