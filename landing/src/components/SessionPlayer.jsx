import { useCallback, useEffect, useRef, useState } from "react";

import recording from "../session.json";

/**
 * A `john` session, played back.
 *
 * Nothing here is drawn by hand. `landing/scripts/record-demo.go` runs
 * terminalui/cmd/demo — the CLI's own terminal UI, driven by a script instead
 * of a backend — on a pseudo-terminal, types at it, and records every byte it
 * wrote with the time it wrote it; session.json is that recording, rendered by
 * the same kind of small terminal emulator the CLI's own tests are written
 * against. So what plays below is the program's output — the tool lines, the
 * proxy holding a request open, the diff viewer — and not a drawing of it.
 *
 * It plays once when it scrolls into view and then rests on the last frame,
 * which is also what a visitor without JavaScript and anyone who asked for
 * reduced motion gets: the finished session, scrollable, from the first render.
 *
 * The panel is dark in both of the page's themes on purpose. The CLI's colours
 * are adaptive, so a recording carries the palette of whatever terminal it was
 * made against — mixing a light page with a dark recording would be showing
 * somebody else's terminal.
 */

const { cols, rows: height, title, frames, styles } = recording;

/** apply lays one frame's changed rows over the screen before it. */
function apply(rows, frame) {
  const next = rows.slice(0, frame.n);
  while (next.length < frame.n) next.push([]);
  for (const [index, runs] of frame.r || []) next[index] = runs;
  return next;
}

const FINAL = frames.reduce(apply, []);

const ENTITIES = { "&": "&amp;", "<": "&lt;", ">": "&gt;" };
const escape = (s) => s.replace(/[&<>]/g, (c) => ENTITIES[c]);

/**
 * A terminal's grid only survives in the browser while every character takes
 * exactly one cell, and outside plain ASCII that cannot be relied on: the box
 * drawing, arrows and glyphs the CLI draws with are missing from some system
 * monospace fonts, and the font the browser falls back to renders them at its
 * own width — twice a cell for the box characters on a machine that falls back
 * to a CJK face, half a cell for the middle dot. One wrong glyph and the box
 * around the approval request stops being a box.
 *
 * So anything outside ASCII is given a cell of its own to sit in (.cast-cell
 * in the stylesheet), which fixes the advance at 1ch whatever the glyph
 * believes it is worth. ASCII is left as plain text, which is most of the
 * transcript and costs nothing.
 */
const ASCII = /^[ -~]*$/;

const span = (className, text) =>
  !text ? "" : className ? `<span class="${className}">${text}</span>` : text;

/** cells renders one run, boxing everything outside ASCII into its own cell. */
function cells(text, className) {
  const cell = className ? `${className} cast-cell` : "cast-cell";
  let out = "";
  let plain = "";
  for (const ch of text) {
    if (ch >= " " && ch <= "~") {
      plain += ch;
      continue;
    }
    out += span(className, escape(plain));
    plain = "";
    out += `<span class="${cell}">${ch}</span>`;
  }
  return out + span(className, escape(plain));
}

/**
 * markup renders the screen in one pass. It is a few thousand spans and nearly
 * all of them change from frame to frame, so it is built as one string and
 * handed to the DOM whole rather than reconciled element by element.
 */
function markup(rows) {
  let out = "";
  for (let i = 0; i < rows.length; i++) {
    if (i) out += "\n";
    for (const [style, text] of rows[i]) {
      const className = style ? `tm${style}` : "";
      out += ASCII.test(text)
        ? span(className, escape(text))
        : cells(text, className);
    }
  }
  return out;
}

/** The recorded renditions, as one stylesheet. Index 0 is the plain default. */
const PALETTE_CSS = styles
  .map((s, i) => {
    if (!i) return "";
    const out = [];
    if (s.f) out.push(`color:${s.f}`);
    if (s.g) out.push(`background:${s.g}`);
    if (s.b) out.push("font-weight:600");
    if (s.d) out.push("opacity:.65");
    if (s.i) out.push("font-style:italic");
    if (s.u) out.push("text-decoration:underline");
    return `.cast .tm${i}{${out.join(";")}}`;
  })
  .join("");

export default function SessionPlayer() {
  const [rows, setRows] = useState(FINAL);
  const [playing, setPlaying] = useState(false);
  const screen = useRef(null);
  const timer = useRef(0);
  const live = useRef(FINAL);

  const stop = useCallback(() => {
    cancelAnimationFrame(timer.current);
    timer.current = 0;
  }, []);

  const play = useCallback(() => {
    stop();
    let index = 0;
    let started = 0;
    live.current = [];
    setPlaying(true);

    const step = (now) => {
      started ||= now;
      const elapsed = (now - started) / 1000;
      let screenRows = live.current;
      let moved = false;
      while (index < frames.length && frames[index].t <= elapsed) {
        screenRows = apply(screenRows, frames[index]);
        index++;
        moved = true;
      }
      if (moved) {
        live.current = screenRows;
        setRows(screenRows);
      }
      if (index < frames.length) {
        timer.current = requestAnimationFrame(step);
      } else {
        timer.current = 0;
        setPlaying(false);
      }
    };
    timer.current = requestAnimationFrame(step);
  }, [stop]);

  // Autoplay, once, when the panel is actually on screen — and never for a
  // visitor who asked for reduced motion, who keeps the finished session.
  useEffect(() => {
    const node = screen.current;
    if (!node) return undefined;
    if (window.matchMedia?.("(prefers-reduced-motion: reduce)").matches) {
      return undefined;
    }
    if (!window.IntersectionObserver) {
      play();
      return stop;
    }

    const observer = new IntersectionObserver(
      (entries) => {
        if (!entries.some((e) => e.isIntersecting)) return;
        observer.disconnect();
        play();
      },
      { threshold: 0.25 },
    );
    observer.observe(node);
    return () => {
      observer.disconnect();
      stop();
    };
  }, [play, stop]);

  // A terminal follows its own output; so does this, while it is producing it.
  useEffect(() => {
    if (playing && screen.current) {
      screen.current.scrollTop = screen.current.scrollHeight;
    }
  }, [rows, playing]);

  return (
    <figure className="cast" style={{ "--cast-cols": cols, "--cast-rows": height }}>
      {/* href + precedence is what asks React to hoist this into <head> and
          to emit it exactly once, rendered and prerendered alike. */}
      <style href="cast-palette" precedence="default">
        {PALETTE_CSS}
      </style>

      {/* A window's title bar. The title is not written here: it is read off
          the session's own status line while it records, so it cannot end up
          naming a directory the recording never opened. */}
      <figcaption className="cast-bar">
        <span className="cast-lead">
          <span className="cast-lights" aria-hidden="true">
            <i />
            <i />
            <i />
          </span>
        </span>
        <span className="cast-title">{title}</span>
        <button
          type="button"
          className="cast-replay"
          onClick={play}
          disabled={playing}
        >
          {playing ? "playing" : "replay"}
        </button>
      </figcaption>

      <div
        className="cast-screen"
        ref={screen}
        tabIndex={0}
        role="group"
        aria-label="Recorded john session transcript"
      >
        <pre dangerouslySetInnerHTML={{ __html: markup(rows) }} />
      </div>
    </figure>
  );
}
