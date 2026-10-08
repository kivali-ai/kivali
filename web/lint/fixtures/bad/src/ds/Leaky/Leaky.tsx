// Fixture: src/ds is exempt from the element, style= and Radix rules, not from the designer's
// literal and prop rules. Not part of the app.
export function Leaky() {
  return <div className="kv-leaky" data-gap={"6px"} />; // rule: syntax/literal (raw px inside src/ds)
}
