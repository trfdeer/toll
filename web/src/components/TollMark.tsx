// TollMark renders the favicon mark inline so the UI shell can reuse it.
// Geometry mirrors web/public/favicon.svg — an ink tile, the lowercase "t"
// post, and the amber barrier arm.
interface TollMarkProps {
  size?: number;
}

export default function TollMark({ size = 20 }: TollMarkProps) {
  return (
    <svg
      className="toll-mark"
      width={size}
      height={size}
      viewBox="0 0 32 32"
      aria-hidden="true"
      focusable="false"
    >
      <rect
        x="0.5"
        y="0.5"
        width="31"
        height="31"
        rx="7"
        fill="#161616"
        stroke="#ffffff"
        strokeOpacity="0.12"
      />
      <rect x="13.3" y="5" width="5.4" height="22" rx="2.7" fill="#f4f4f4" />
      <rect x="7.5" y="10.3" width="20" height="5.4" rx="2.7" fill="#f1c21b" />
    </svg>
  );
}