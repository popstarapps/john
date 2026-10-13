import { StrictMode } from "react";
import { createRoot, hydrateRoot } from "react-dom/client";
import App from "./App.jsx";
import "./styles.css";

const root = document.getElementById("root");
const tree = (
  <StrictMode>
    <App />
  </StrictMode>
);

// `npm run build:static` bakes the page into index.html, so the markup is
// already here and we hydrate it. Every other build ships an empty root and
// renders from scratch.
if (root.firstChild) {
  hydrateRoot(root, tree);
} else {
  createRoot(root).render(tree);
}
