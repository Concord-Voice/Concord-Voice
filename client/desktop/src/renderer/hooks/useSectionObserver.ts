import { useEffect, useRef } from 'react';

/**
 * Scroll-spy for settings-style pages: observes every `[id^="section-"]`
 * element inside the enclosing `.settings-page-content` scroll container and
 * reports the topmost visible section id (with the `section-` prefix
 * stripped) whenever visibility changes. Shared by SettingsPage and
 * ServerSettingsPage — the two settings surfaces must track sub-navigation
 * identically.
 *
 * A persistent map of all currently-intersecting sections is kept so the
 * IntersectionObserver callback (which only receives *changed* entries) can
 * always pick the topmost visible one. The observer is created inside a small
 * post-render delay so sections have rendered after a tab switch, and is held
 * in an effect-scoped variable so the cleanup disconnects it directly (no DOM
 * expando — #483/#484).
 *
 * `onActiveSubsection` participates in the effect dependencies — pass a
 * stable function (a `useState` setter or `useCallback`) or the observer is
 * torn down and rebuilt every render.
 */
export function useSectionObserver(
  contentRef: React.RefObject<HTMLElement | null>,
  activeSection: string,
  onActiveSubsection: (sectionId: string) => void
): void {
  const visibleSectionsRef = useRef<Map<string, IntersectionObserverEntry>>(new Map());

  useEffect(() => {
    visibleSectionsRef.current.clear();
    const root = contentRef.current?.closest('.settings-page-content') as HTMLElement | null;
    if (!root) return;

    // Created inside the post-render delay below; held in this effect-scoped
    // variable so the cleanup disconnects it directly.
    let observer: IntersectionObserver | null = null;

    // Small delay to let sections render after tab switch
    const timer = setTimeout(() => {
      observer = new IntersectionObserver(
        (entries) => {
          for (const entry of entries) {
            const id = entry.target.id;
            if (entry.isIntersecting) {
              visibleSectionsRef.current.set(id, entry);
            } else {
              visibleSectionsRef.current.delete(id);
            }
          }

          // Pick the topmost visible section (smallest boundingClientRect.top)
          let best: IntersectionObserverEntry | null = null;
          for (const entry of visibleSectionsRef.current.values()) {
            if (!best || entry.boundingClientRect.top < best.boundingClientRect.top) {
              best = entry;
            }
          }
          if (best) {
            onActiveSubsection(best.target.id.replace('section-', ''));
          }
        },
        { root, threshold: [0, 0.1, 0.25, 0.5], rootMargin: '-10% 0px -50% 0px' }
      );

      const sections = root.querySelectorAll('[id^="section-"]');
      for (const el of sections) observer.observe(el);
    }, 50);

    return () => {
      clearTimeout(timer);
      observer?.disconnect();
    };
  }, [activeSection, contentRef, onActiveSubsection]);
}
