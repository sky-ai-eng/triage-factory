import * as RadixSlider from '@radix-ui/react-slider'

interface SliderProps {
  value: number
  onChange: (value: number) => void
  min?: number
  max?: number
  step?: number
  label: string
}

export default function Slider({
  value,
  onChange,
  min = 0,
  max = 1,
  step = 0.05,
  label,
}: SliderProps) {
  return (
    <RadixSlider.Root
      value={[value]}
      onValueChange={([v]) => onChange(v)}
      min={min}
      max={max}
      step={step}
      aria-label={label}
      className="relative flex items-center select-none touch-none w-full h-5"
    >
      <RadixSlider.Track className="relative grow rounded-full h-[4px] bg-tint-3">
        <RadixSlider.Range className="absolute rounded-full h-full bg-warm/40" />
      </RadixSlider.Track>
      <RadixSlider.Thumb className="block w-4 h-4 rounded-full bg-warm border-2 border-line-1 shadow-[0_1px_4px_rgba(0,0,0,0.12),0_0_0_1px_rgba(0,0,0,0.04)] hover:shadow-[0_1px_6px_rgba(0,0,0,0.16),0_0_0_3px_var(--color-warm-2)] transition-shadow focus:outline-none" />
    </RadixSlider.Root>
  )
}
