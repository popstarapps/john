import { useEffect } from "react";

/**
 * Fades each top-level block of every section in as it scrolls into view.
 * Skipped entirely when the visitor asked for reduced motion.
 */
export function useReveal() {
  useEffect(() => {
    if (window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;

    const targets = Array.from(
      document.querySelectorAll(
        ".band > .wrap > *, .hero > .wrap > *, .ref-head > .wrap > *",
      ),
    );
    targets.forEach((el, i) => {
      el.classList.add("rise");
      el.style.transitionDelay = `${Math.min(i, 3) * 60}ms`;
    });

    const io = new IntersectionObserver(
      (entries) => {
        entries.forEach((entry) => {
          if (entry.isIntersecting) {
            entry.target.classList.add("in");
            io.unobserve(entry.target);
          }
        });
      },
      { rootMargin: "0px 0px -8% 0px", threshold: 0.05 },
    );
    targets.forEach((el) => io.observe(el));

    return () => io.disconnect();
  }, []);
}
