import { Input as InputPrimitive } from "@base-ui/react/input"
import type * as React from "react"

import { cn } from "@/lib/utils"

function Input({ className, type, ...props }: React.ComponentProps<"input">) {
  return (
    <InputPrimitive
      type={type}
      data-slot="input"
      className={cn(
        "border-input bg-background/88 placeholder:text-muted-foreground/72 focus-visible:border-primary/55 focus-visible:bg-background focus-visible:ring-primary/10 h-12 w-full min-w-0 rounded-xl border px-4 py-2 text-base shadow-[0_1px_0_rgba(255,255,255,.4)_inset] transition-[border-color,box-shadow,background-color] outline-none focus-visible:ring-4 disabled:pointer-events-none disabled:cursor-not-allowed disabled:opacity-50",
        className
      )}
      {...props}
    />
  )
}

export { Input }
