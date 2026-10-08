// Fixture: a file that does not parse. The lint run must FAIL (a crashed check never passes).
export function Broken() {
  return <div>;
}
