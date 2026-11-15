package main

import (
	"context"
	"strings"
	"testing"
)

// receiptHTML is shaped like the in-store receipts that came back with an
// empty body: a PredictSpring template with a script-heavy <head>, CSS, an
// Outlook conditional comment and nested layout tables. The order, address
// and card details are made up; the structure is the point.
const receiptHTML = `<!DOCTYPE html PUBLIC "-//W3C//DTD XHTML 1.0 Strict//EN" "http://www.w3.org/TR/xhtml1/DTD/xhtml1-strict.dtd">
<html xmlns="http://www.w3.org/1999/xhtml">
  <head>
    <meta http-equiv="Content-Type" content="text/html; charset=UTF-8" />
    <title>Order</title>
    <script src="https://cdn.example.com/JsBarcode.all.min.js"></script>
    <script type="text/javascript">
      window.onload = function updateEan() {
        var ean = $("#ean").val();
        if (ean.length < 1 && ean.length > 13) { return; }
      };
    </script>
    <style type="text/css">
      body { font-family: Arial, Helvetica, sans-serif !important; }
      table { border-collapse: collapse; }
    </style>
    <!--
  Outlook Conditional CSS
  -->
    <!--[if mso 12]>
      <style type="text/css">.flexibleContainer { display: block !important; }</style>
    <![endif]-->
  </head>
  <body bgcolor="#ffffff">
<table id="bodyTable"><tbody><tr><td id="bodyCell" align="center">
<table id="emailBody"><tbody>
<tr><td><h3><strong><span>Thank you for shopping with us!</span></strong></h3></td></tr>
<tr><td class="textContent"><hr></td></tr>
<tr><td class="textContent">
<p style="text-align: center;">Your receipt of order&nbsp;<strong>#<span>PS00000001&nbsp;</span></strong></p>
</td></tr>
<tr><td>
<table border="1">
<thead>
<tr><td>Order #</td><td>Store #</td><td>Register #</td></tr>
</thead>
<tbody>
<tr><td><p></p><p>PS00000001</p></td><td><p>042</p></td><td><p>042001</p></td></tr>
</tbody>
</table>
</td></tr>
<tr><td>
<table><tbody>
<tr><td><p>ITEM</p></td><td>DESCRIPTION</td><td>QTY</td><td>PRICE</td><td>SUBTOTAL</td></tr>
</tbody></table>
<table><tbody>
<tr>
<td><p><a href="{ProductUrl}"><img src="http://assets.example.com/item.png" alt="Product" onerror="this.src='x'"></a></p></td>
<td><p><span>Men s PRO HD Fleece Lined Underwader Pants</span></p>
<p>SKU:&nbsp;<span>3GLG1052</span>&nbsp;</p></td>
<td><span>1</span></td><td><span>$169.00</span></td><td><span>$</span>169.00</td>
</tr>
</tbody></table>
</td></tr>
<tr><td>
<table><tbody>
<tr><td>Subtotal:</td><td><span>$169.00&nbsp;</span></td></tr>
<tr><td>NY STATE TAX - 0.04%</td><td><span>$6.76</span></td></tr>
<tr><td>NY CITY TAX - 0.045%</td><td><span>$7.61</span></td></tr>
<tr><td>NY SPECIAL TAX - 0.00375%</td><td><span>$0.63</span></td></tr>
<tr><td>Total:</td><td><span>$184.00&nbsp;</span></td></tr>
<tr><td><p>Payment Method:</p></td><td><p>CreditCard Example 0000</p></td></tr>
</tbody></table>
</td></tr>
<tr><td><p><span style="color: rgb(255, 255, 255); font-size: 9px;">Copyright &copy;2024 Example Inc. All Rights Reserved</span></p></td></tr>
</tbody></table>
</td></tr></tbody></table>
</body>
</html>`

const receiptText = `Thank you for shopping with us!

Your receipt of order #PS00000001
Order # | Store # | Register #
PS00000001 | 042 | 042001

ITEM | DESCRIPTION | QTY | PRICE | SUBTOTAL

Men s PRO HD Fleece Lined Underwader Pants SKU: 3GLG1052 | 1 | $169.00 | $169.00

Subtotal: | $169.00
NY STATE TAX - 0.04% | $6.76
NY CITY TAX - 0.045% | $7.61
NY SPECIAL TAX - 0.00375% | $0.63
Total: | $184.00
Payment Method: | CreditCard Example 0000

Copyright ©2024 Example Inc. All Rights Reserved`

// The receipt as Gmail parses it: multipart/alternative with an empty
// text/plain part beside the HTML. Preferring the empty part was the bug.
func receiptPayload(html string) gmailPart {
	empty := gmailPart{MimeType: "text/plain", Headers: []gmailHeader{{"Content-Type", "text/plain; charset=UTF-8"}}}
	empty.Body.Data = b64("\r\n")
	h := gmailPart{MimeType: "text/html", Headers: []gmailHeader{{"Content-Type", "text/html; charset=UTF-8"}}}
	h.Body.Data = b64(html)
	return gmailPart{MimeType: "multipart/alternative", Parts: []gmailPart{empty, h}}
}

func TestExtractBodyHTMLReceiptBesideEmptyPlain(t *testing.T) {
	got, atts := extractBody(receiptPayload(receiptHTML))
	if got != receiptText {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, receiptText)
	}
	if len(atts) != 0 {
		t.Errorf("attachments = %+v", atts)
	}
	for _, leak := range []string{"JsBarcode", "window.onload", "font-family", "Outlook", "mso", "<", "Order\n"} {
		if strings.Contains(got, leak) {
			t.Errorf("%q leaked into %q", leak, got)
		}
	}
}

// HTML-only with no text/plain at all, nested a level down the way a mail
// with inline images arrives: multipart/mixed > multipart/related > text/html.
func TestExtractBodyNestedHTMLOnly(t *testing.T) {
	h := gmailPart{MimeType: "TEXT/HTML"}
	h.Body.Data = b64(receiptHTML)
	img := gmailPart{MimeType: "image/png", Filename: "logo.png"}
	img.Body.Size = 10
	p := gmailPart{MimeType: "multipart/mixed", Parts: []gmailPart{
		{MimeType: "multipart/related", Parts: []gmailPart{h, img}},
	}}
	got, atts := extractBody(p)
	if got != receiptText {
		t.Errorf("got:\n%s", got)
	}
	if len(atts) != 1 || atts[0].Filename != "logo.png" {
		t.Errorf("attachments = %+v", atts)
	}
}

// A non-empty text/plain part still wins, byte for byte as before.
func TestExtractBodyPrefersRealPlainText(t *testing.T) {
	p := receiptPayload(receiptHTML)
	p.Parts[0].Body.Data = b64("  Thanks for your order.\r\nTotal $184.00\r\n")
	if got, _ := extractBody(p); got != "Thanks for your order.\r\nTotal $184.00" {
		t.Errorf("got %q", got)
	}
}

func TestHTMLToTextHiddenElements(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		// One alternation regex ended <head> at the first </title>, leaving
		// the rest of the head behind.
		"head ends at its own close": {`<head><title>t</title>meta junk<style>x</style></head><p>body</p>`, "body"},
		"unclosed script":            {`<p>before</p><script>steal()`, "before"},
		"header is not head":         {`<header>top</header><head>h</head>`, "top"},
		"uppercase tags":             {`<SCRIPT type="x">a()</SCRIPT><P>shown</P>`, "shown"},
		"self-closing attributes":    {`<script/>x()</script>ok`, "ok"},
		"forged cell marker":         {"ab", "ab"},
	} {
		if got := htmlToText(tc.in); got != tc.want {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// paddedReceiptHTML is the receipt with its CSS padded well past the
// 20,000 character cap.
func paddedReceiptHTML() string {
	return strings.Replace(receiptHTML, "</style>", strings.Repeat("td { color: red; }\n", 2000)+"</style>", 1)
}

// End to end through read_thread: the cap applies to the converted text, not
// the markup, so a receipt whose HTML is far over 20,000 characters still
// comes back whole and untruncated.
func TestReadThreadHTMLReceipt(t *testing.T) {
	if n := len(paddedReceiptHTML()); n <= maxBodyChars {
		t.Fatalf("fixture is only %d bytes", n)
	}
	th, err := newFakeGmail(t).client().readThread(context.Background(), "receipt")
	if err != nil {
		t.Fatal(err)
	}
	if got := th.Messages[0].Body; got != receiptText {
		t.Errorf("got:\n%s", got)
	}
}
