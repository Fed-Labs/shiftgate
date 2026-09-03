import type { Metadata } from "next";
import { Space_Grotesk, IBM_Plex_Mono } from "next/font/google";
import { Providers } from "@/lib/providers";
import "./globals.css";

const display = Space_Grotesk({
  variable: "--font-display",
  subsets: ["latin"],
  weight: ["400", "500", "600", "700"],
});

const mono = IBM_Plex_Mono({
  variable: "--font-mono",
  subsets: ["latin"],
  weight: ["400", "500"],
});

export const metadata: Metadata = {
  title: {
    default: "SHIFT — Move supported computation between Linux machines",
    template: "%s — SHIFTGATE",
  },
  description:
    "SHIFT moves running computation between Linux machines with encrypted checkpoints, validated restore, and explicit compatibility boundaries.",
  metadataBase: new URL("https://shiftgate.dev"),
  icons: {
    icon: "/favicon.png",
    apple: "/favicon.png",
  },
  openGraph: {
    title: "SHIFTGATE",
    description: "Move supported computation between Linux machines with validated checkpoint/restore.",
    type: "website",
    locale: "en_US",
    images: [
      {
        url: "/logo.png",
        width: 1200,
        height: 630,
        alt: "SHIFTGATE",
      },
    ],
  },
  twitter: {
    card: "summary_large_image",
    title: "SHIFTGATE",
    description: "Move computation between machines.",
    images: ["/logo.png"],
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
