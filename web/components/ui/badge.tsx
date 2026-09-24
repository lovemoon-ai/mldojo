import * as React from "react";
import { cva, type VariantProps } from "class-variance-authority";
import { cn } from "@/lib/utils";

export const badgeVariants = cva(
  "inline-flex items-center gap-1 whitespace-nowrap rounded-md border px-1.5 py-0.5 text-xs font-medium leading-4 [&_svg]:size-3 [&_svg]:shrink-0",
  {
    variants: {
      variant: {
        default: "border-transparent bg-primary text-primary-foreground",
        secondary: "border-transparent bg-muted text-foreground",
        outline: "border-border text-foreground",
        muted: "border-transparent bg-muted text-muted-foreground",
        success: "border-emerald-600/20 bg-emerald-500/10 text-emerald-700 dark:text-emerald-400",
        warning: "border-amber-600/20 bg-amber-500/10 text-amber-700 dark:text-amber-400",
        danger: "border-red-600/20 bg-red-500/10 text-red-700 dark:text-red-400",
        info: "border-blue-600/20 bg-blue-500/10 text-blue-700 dark:text-blue-400",
        violet: "border-violet-600/20 bg-violet-500/10 text-violet-700 dark:text-violet-400",
      },
    },
    defaultVariants: { variant: "secondary" },
  },
);

export type BadgeVariant = NonNullable<VariantProps<typeof badgeVariants>["variant"]>;

export interface BadgeProps extends React.HTMLAttributes<HTMLSpanElement>, VariantProps<typeof badgeVariants> {}

export function Badge({ className, variant, ...props }: BadgeProps) {
  return <span className={cn(badgeVariants({ variant }), className)} {...props} />;
}
