// lucide-react v1.x dropped brand marks. Inline SVG of the Jira mark so the
// Jira settings tab keeps a recognizable icon in the sidebar and headers.
export function JiraMark({ className }: { className?: string }) {
  return (
    <svg viewBox="0 0 24 24" aria-hidden="true" className={className} fill="currentColor">
      <path d="M11.53 2c0 2.4 1.97 4.35 4.35 4.35h1.78v1.72c0 2.4 1.94 4.34 4.34 4.34V2.84A.84.84 0 0 0 21.16 2H11.53Z" transform="translate(0 -0.5)" />
      <path d="M6.77 6.77c0 2.4 1.95 4.34 4.35 4.34h1.78v1.73c0 2.4 1.94 4.34 4.34 4.34V7.6a.83.83 0 0 0-.83-.83H6.77Z" transform="translate(-1.7 -1.7)" />
      <path d="M2 11.53c0 2.4 1.95 4.35 4.35 4.35h1.78v1.72c0 2.4 1.94 4.34 4.34 4.34v-9.58a.83.83 0 0 0-.83-.83H2Z" />
    </svg>
  );
}
