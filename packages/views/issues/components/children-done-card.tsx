"use client";

import { CircleCheck, ChevronDown } from "lucide-react";
import { Button } from "@multica/ui/components/ui/button";
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@multica/ui/components/ui/dropdown-menu";
import { useChildrenDoneAction, useUpdateIssue } from "@multica/core/issues/mutations";
import type { Issue, IssueStatus, OnChildrenDone } from "@multica/core/types";
import { toast } from "sonner";
import { useT } from "../../i18n";

/** Mirrors isDeliveredChildStatus on the server. */
function isDelivered(status: string): boolean {
  return status === "done" || status === "cancelled" || status === "in_review";
}

function isAccepted(status: string): boolean {
  return status === "done" || status === "cancelled";
}

/** Every child has handed its work over. Mirrors childrenAllDelivered. */
export function allChildrenDelivered(children: Issue[]): boolean {
  return children.length > 0 && children.every((c) => isDelivered(c.status));
}

/**
 * The status a parent may roll up to — mirrors rollupStatusForParent.
 *
 * A parent never rolls up past its weakest child: every child accepted means
 * the parent may reach `done`; a child that is only delivered caps the parent
 * at `in_review`. Rendering the computed target in the button label is what
 * makes that rule legible instead of surprising.
 */
export function rollupStatusForChildren(children: Issue[]): IssueStatus {
  return children.every((c) => isAccepted(c.status)) ? "done" : "in_review";
}

const POLICIES: OnChildrenDone[] = ["auto", "wake", "notify", "close", "off"];

/** Explicit key-by-key lookup — the i18n accessor is a typed property path,
 *  so a variable index would erase the key checking that makes a missing
 *  translation a compile error. */
function usePolicyLabels(): Record<OnChildrenDone, { label: string; hint: string }> {
  const { t } = useT("issues");
  return {
    auto: {
      label: t(($) => $.children_done.policy.options.auto.label),
      hint: t(($) => $.children_done.policy.options.auto.hint),
    },
    wake: {
      label: t(($) => $.children_done.policy.options.wake.label),
      hint: t(($) => $.children_done.policy.options.wake.hint),
    },
    notify: {
      label: t(($) => $.children_done.policy.options.notify.label),
      hint: t(($) => $.children_done.policy.options.notify.hint),
    },
    close: {
      label: t(($) => $.children_done.policy.options.close.label),
      hint: t(($) => $.children_done.policy.options.close.hint),
    },
    off: {
      label: t(($) => $.children_done.policy.options.off.label),
      hint: t(($) => $.children_done.policy.options.off.hint),
    },
  };
}

/**
 * The in-flow decision the notify policy raises: all sub-issues are delivered,
 * so what happens to the parent?
 *
 * This card is the reason the feature needs no up-front configuration. The
 * click IS the decision — nothing has to be set before the tree runs — and it
 * is asked in context, right above the sub-issue list it is about, rather than
 * in a settings page the user would have to visit first.
 */
export function ChildrenDoneCard({
  issue,
  children,
}: {
  issue: Issue;
  children: Issue[];
}) {
  const { t } = useT("issues");
  const action = useChildrenDoneAction(issue.id);
  const target = rollupStatusForChildren(children);
  // Explicit branch, not a dynamic index: the rollup can only ever produce
  // these two statuses (see rollupStatusForChildren).
  const targetLabel =
    target === "done" ? t(($) => $.status.done) : t(($) => $.status.in_review);

  const toastLabels: Record<"continue" | "close" | "dismiss", string> = {
    continue: t(($) => $.children_done.toast.continue),
    close: t(($) => $.children_done.toast.close),
    dismiss: t(($) => $.children_done.toast.dismiss),
  };

  const run = (kind: "continue" | "close" | "dismiss") => {
    action.mutate(kind, {
      onSuccess: () => {
        toast.success(toastLabels[kind]);
      },
      onError: () => {
        toast.error(t(($) => $.children_done.toast.failed));
      },
    });
  };

  return (
    <div className="mb-3 rounded-lg border border-brand/30 bg-brand/5 px-3.5 py-3">
      <div className="flex items-start gap-2.5">
        <CircleCheck className="mt-0.5 h-4 w-4 shrink-0 text-brand" />
        <div className="min-w-0 flex-1">
          <p className="text-body font-medium text-foreground">
            {t(($) => $.children_done.title, { count: children.length })}
          </p>
          <p className="mt-0.5 text-caption text-muted-foreground">
            {t(($) => $.children_done.description)}
          </p>
          <div className="mt-2.5 flex flex-wrap items-center gap-2">
            <Button
              size="sm"
              variant="default"
              disabled={action.isPending}
              onClick={() => run("close")}
            >
              {t(($) => $.children_done.close, { status: targetLabel })}
            </Button>
            <Button
              size="sm"
              variant="outline"
              disabled={action.isPending}
              onClick={() => run("continue")}
            >
              {t(($) => $.children_done.continue)}
            </Button>
            <Button
              size="sm"
              variant="ghost"
              disabled={action.isPending}
              onClick={() => run("dismiss")}
            >
              {t(($) => $.children_done.dismiss)}
            </Button>
          </div>
        </div>
      </div>
    </div>
  );
}

/**
 * Standing override for the inferred policy, in the Sub-issues header.
 *
 * Deliberately secondary to the card above: `auto` is the default and is
 * expected to stay the default for nearly every issue. This exists for the
 * minority case where the user knows the inference is wrong for this tree and
 * wants to stop being asked.
 */
export function ChildrenDonePolicyMenu({ issue }: { issue: Issue }) {
  const { t } = useT("issues");
  const labels = usePolicyLabels();
  const updateIssue = useUpdateIssue();
  const current: OnChildrenDone = issue.on_children_done ?? "auto";

  return (
    <DropdownMenu>
      <DropdownMenuTrigger
        render={
          <button
            type="button"
            className="inline-flex h-7 items-center gap-1 rounded-md px-1.5 text-caption text-muted-foreground transition-colors hover:bg-accent hover:text-foreground"
            aria-label={t(($) => $.children_done.policy.aria)}
          >
            <span>{labels[current].label}</span>
            <ChevronDown className="h-3 w-3" />
          </button>
        }
      />
      <DropdownMenuContent align="end" className="w-72">
        {POLICIES.map((policy) => (
          <DropdownMenuItem
            key={policy}
            onClick={() => {
              if (policy === current) return;
              updateIssue.mutate({ id: issue.id, on_children_done: policy });
            }}
            className="flex-col items-start gap-0.5 py-2"
          >
            <span
              className={
                policy === current ? "font-medium text-foreground" : "text-foreground"
              }
            >
              {labels[policy].label}
            </span>
            <span className="text-caption text-muted-foreground">
              {labels[policy].hint}
            </span>
          </DropdownMenuItem>
        ))}
      </DropdownMenuContent>
    </DropdownMenu>
  );
}
