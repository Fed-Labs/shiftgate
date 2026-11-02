import type { Metadata } from "next";
import { Space_Grotesk, IBM_Plex_Mono } from "next/font/google";
import { Providers } from "@/lib/providers";
import "./globals.css";

const SITE_URL = process.env.NEXT_PUBLIC_SITE_URL || "https://shiftgate.dev";

const display = Space_Grotesk({
  variable: "--font-shift-display",
  subsets: ["latin"],
  weight: ["400", "500", "600", "700"],
});

const mono = IBM_Plex_Mono({
  variable: "--font-shift-mono",
  subsets: ["latin"],
  weight: ["400", "500"],
});

export const metadata: Metadata = {
  title: {
    default: "SHIFT — Move supported computation between Linux machines",
    template: "%s — SHIFT",
  },
  description:
    "SHIFT moves running computation between Linux machines with encrypted checkpoints, validated restore, and explicit compatibility boundaries.",
  metadataBase: new URL(SITE_URL),
  icons: {
    icon: "/favicon.png",
    apple: "/favicon.png",
  },
  openGraph: {
    title: "SHIFT",
    description: "Move supported computation between Linux machines with validated checkpoint/restore.",
    type: "website",
    locale: "en_US",
    images: [
      {
        url: "/og.png",
        width: 1200,
        height: 630,
        alt: "SHIFT",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title: "SHIFT",
    description: "Move computation between machines.",
    images: ["/og.png"],
  },
  robots: {
    index: true,
    follow: true,
  },
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html
      lang="en"
      className={`${display.variable} ${mono.variable} h-full antialiased`}
    >
      <body className="min-h-full flex flex-col bg-bg text-text">
        <Providers>{children}</Providers>
      </body>
    </html>
  );
}
