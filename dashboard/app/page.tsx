// Root route — redirect to the welcome splash. The Go server also
// redirects "/" to "/welcome" at the HTTP layer, but doing it here
// too means client-side navigation (e.g. via the brand link) lands
// on /welcome immediately without a server roundtrip.

import { redirect } from "next/navigation";

export default function HomePage(): never {
  redirect("/welcome");
}