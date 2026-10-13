/**
 * A full-width band with the standard measure.
 * The light/dark alternation is handled in CSS by position, not by a prop,
 * so sections can be reordered or removed without restriping the page.
 */
export default function Section({ id, children }) {
  return (
    <section className="band" id={id}>
      <div className="wrap">{children}</div>
    </section>
  );
}
