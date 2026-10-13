import { useReveal } from "./useReveal.js";

import TopBar from "./components/TopBar.jsx";

import Hero from "./sections/Hero.jsx";
import WhatItIs from "./sections/WhatItIs.jsx";
import ConsoleTour from "./sections/ConsoleTour.jsx";
import DevExperience from "./sections/DevExperience.jsx";
import Architecture from "./sections/Architecture.jsx";
import Trifecta from "./sections/Trifecta.jsx";
import Features from "./sections/Features.jsx";
import Faq from "./sections/Faq.jsx";
import GetStarted from "./sections/GetStarted.jsx";
import Footer from "./sections/Footer.jsx";

// One argument, read top to bottom: what John is, who each half of the room is
// getting, how it is built, what it replaces, the risk it is actually built
// around, what you can express with it, and what it takes to run — ending on
// the thing to do next.
//
// The page stripes automatically (see `main > section:nth-of-type(even)` in
// styles.css), so sections can be reordered or cut without restriping. What
// that does mean is that adding or removing one flips the shade of everything
// after it; check the stripe if a section moves.
export default function App() {
  useReveal();

  return (
    <>
      <TopBar />

      {/* tabIndex so the skip link reliably moves focus, not just the viewport. */}
      <main id="main" tabIndex={-1}>
        <span id="top" />
        <Hero />
        <WhatItIs />
        <ConsoleTour />
        <DevExperience />
        <Architecture />
        <Trifecta />
        <Features />
        <Faq />
        <GetStarted />
      </main>

      <Footer />
    </>
  );
}
