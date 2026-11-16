package app

import (
	"fmt"
	"strings"
)

// phoneCountry identifies an accepted calling country.
type phoneCountry struct{ Name, CallingCode string }

const (
	// usOrCanada is the country name of a North American number in the US
	// or Canada; MMHotline treats both the same way.
	usOrCanada = "the US or Canada"
)

// acceptedPhoneCountries are the countries accepted at launch: the US,
// Canada, the 27 EU countries, the UK, Norway, Switzerland, and Iceland.
var acceptedPhoneCountries = []phoneCountry{
	{usOrCanada, "1"},
	{"Austria", "43"},
	{"Belgium", "32"},
	{"Bulgaria", "359"},
	{"Croatia", "385"},
	{"Cyprus", "357"},
	{"Czechia", "420"},
	{"Denmark", "45"},
	{"Estonia", "372"},
	{"Finland", "358"},
	{"France", "33"},
	{"Germany", "49"},
	{"Greece", "30"},
	{"Hungary", "36"},
	{"Ireland", "353"},
	{"Italy", "39"},
	{"Latvia", "371"},
	{"Lithuania", "370"},
	{"Luxembourg", "352"},
	{"Malta", "356"},
	{"the Netherlands", "31"},
	{"Poland", "48"},
	{"Portugal", "351"},
	{"Romania", "40"},
	{"Slovakia", "421"},
	{"Slovenia", "386"},
	{"Spain", "34"},
	{"Sweden", "46"},
	{"the UK", "44"},
	{"Norway", "47"},
	{"Switzerland", "41"},
	{"Iceland", "354"},
}

// otherNorthAmericanAreaCodes are area codes under +1 that are not in the US
// or Canada: Caribbean countries and US territories, refused like any other
// country. Toll-free and premium codes are refused separately.
var otherNorthAmericanAreaCodes = map[string]string{
	"242": "the Bahamas", "246": "Barbados", "264": "Anguilla", "268": "Antigua and Barbuda",
	"284": "the British Virgin Islands", "340": "the US Virgin Islands", "345": "the Cayman Islands",
	"441": "Bermuda", "473": "Grenada", "649": "the Turks and Caicos Islands", "658": "Jamaica",
	"664": "Montserrat", "670": "the Northern Mariana Islands", "671": "Guam", "684": "American Samoa",
	"721": "Sint Maarten", "758": "Saint Lucia", "767": "Dominica", "784": "Saint Vincent and the Grenadines",
	"787": "Puerto Rico", "809": "the Dominican Republic", "829": "the Dominican Republic",
	"849": "the Dominican Republic", "868": "Trinidad and Tobago", "869": "Saint Kitts and Nevis",
	"876": "Jamaica", "939": "Puerto Rico",
}

// northAmericanTollFreeOrPremium are area codes that are not a person's
// phone: toll-free, premium (900), interexchange (700), and the 5XX codes
// NANPA assigns to personal communications services, which forward to any
// number and may be charged at special rates.
var northAmericanTollFreeOrPremium = map[string]bool{
	"800": true, "833": true, "844": true, "855": true, "866": true, "877": true, "888": true,
	"900": true, "700": true,
	"500": true, "521": true, "522": true, "523": true, "524": true, "525": true, "526": true, "527": true, "528": true, "529": true,
	"532": true, "533": true, "535": true, "538": true, "542": true, "543": true, "544": true, "545": true, "546": true, "547": true,
	"549": true, "550": true, "552": true, "553": true, "554": true, "556": true, "558": true, "566": true, "569": true, "577": true,
	"578": true, "588": true, "589": true,
}

// refusedCallingCodeNames names the countries of common calling codes
// MMHotline does not accept yet, for the refusal message. A code not listed
// here is named by its number.
var refusedCallingCodeNames = map[string]string{
	"7": "Russia or Kazakhstan", "20": "Egypt", "27": "South Africa", "51": "Peru", "52": "Mexico",
	"53": "Cuba", "54": "Argentina", "55": "Brazil", "56": "Chile", "57": "Colombia", "58": "Venezuela",
	"60": "Malaysia", "61": "Australia", "62": "Indonesia", "63": "the Philippines", "64": "New Zealand",
	"65": "Singapore", "66": "Thailand", "81": "Japan", "82": "South Korea", "84": "Vietnam", "86": "China",
	"90": "Turkey", "91": "India", "92": "Pakistan", "93": "Afghanistan", "94": "Sri Lanka", "95": "Myanmar",
	"98": "Iran", "212": "Morocco", "213": "Algeria", "216": "Tunisia", "218": "Libya", "221": "Senegal",
	"233": "Ghana", "234": "Nigeria", "251": "Ethiopia", "254": "Kenya", "255": "Tanzania", "256": "Uganda",
	"260": "Zambia", "262": "Réunion or Mayotte", "263": "Zimbabwe", "297": "Aruba", "298": "the Faroe Islands",
	"299": "Greenland", "350": "Gibraltar", "355": "Albania", "373": "Moldova", "374": "Armenia",
	"375": "Belarus", "376": "Andorra", "377": "Monaco", "378": "San Marino", "380": "Ukraine",
	"381": "Serbia", "382": "Montenegro", "383": "Kosovo", "387": "Bosnia and Herzegovina",
	"389": "North Macedonia", "423": "Liechtenstein", "501": "Belize", "502": "Guatemala",
	"503": "El Salvador", "504": "Honduras", "505": "Nicaragua", "506": "Costa Rica", "507": "Panama",
	"509": "Haiti", "590": "Guadeloupe", "591": "Bolivia", "593": "Ecuador", "594": "French Guiana",
	"595": "Paraguay", "596": "Martinique", "598": "Uruguay", "599": "Curaçao", "852": "Hong Kong",
	"853": "Macau", "855": "Cambodia", "880": "Bangladesh", "886": "Taiwan", "960": "the Maldives",
	"961": "Lebanon", "962": "Jordan", "963": "Syria", "964": "Iraq", "965": "Kuwait", "966": "Saudi Arabia",
	"967": "Yemen", "968": "Oman", "970": "Palestine", "971": "the United Arab Emirates", "972": "Israel",
	"973": "Bahrain", "974": "Qatar", "975": "Bhutan", "976": "Mongolia", "977": "Nepal", "992": "Tajikistan",
	"993": "Turkmenistan", "994": "Azerbaijan", "995": "Georgia", "996": "Kyrgyzstan", "998": "Uzbekistan",
}

// refusedCountryText is the refusal when a number's country is not accepted.
const refusedCountryText = "MMHotline can't call phone numbers in %s yet. Use a Pushover notification instead."

// parsePhoneNumber turns what a person typed into an E.164 number such as
// +12025550123 and finds its country. Spaces, dashes, dots, and
// parentheses are ignored; the number must start with + (or 00) and its
// country code. A number whose country is not accepted is refused with its
// country's name.
func parsePhoneNumber(typed string) (string, phoneCountry, error) {
	cleaned := strings.Map(func(r rune) rune {
		switch r {
		case ' ', '-', '.', '(', ')', ' ':
			return -1
		}
		return r
	}, strings.TrimSpace(typed))
	if strings.HasPrefix(cleaned, "00") {
		cleaned = "+" + cleaned[2:]
	}
	digits := strings.TrimPrefix(cleaned, "+")
	if !strings.HasPrefix(cleaned, "+") || len(digits) < 8 || len(digits) > 15 || strings.Trim(digits, "0123456789") != "" || digits[0] == '0' {
		return "", phoneCountry{}, fmt.Errorf("%w: enter the phone number with its country code, such as +1 202 555 0123 or +44 7700 900123", ErrInvalid)
	}
	number := "+" + digits
	if digits[0] == '1' {
		if len(digits) != 11 || digits[1] < '2' || digits[4] < '2' {
			return "", phoneCountry{}, fmt.Errorf("%w: a number in the US or Canada has 10 digits after +1, such as +1 202 555 0123", ErrInvalid)
		}
		area := digits[1:4]
		if northAmericanTollFreeOrPremium[area] {
			return "", phoneCountry{}, fmt.Errorf("%w: enter a person's phone number, not a toll-free or premium number", ErrInvalid)
		}
		if name, ok := otherNorthAmericanAreaCodes[area]; ok {
			return "", phoneCountry{}, fmt.Errorf("%w: "+refusedCountryText, ErrInvalid, name)
		}
		return number, acceptedPhoneCountries[0], nil
	}
	for length := 3; length >= 1; length-- {
		code := digits[:length]
		for _, c := range acceptedPhoneCountries {
			if c.CallingCode == code {
				return number, c, nil
			}
		}
		if name, ok := refusedCallingCodeNames[code]; ok {
			return "", phoneCountry{}, fmt.Errorf("%w: "+refusedCountryText, ErrInvalid, name)
		}
	}
	return "", phoneCountry{}, fmt.Errorf("%w: "+refusedCountryText, ErrInvalid, "the country with calling code +"+digits[:min(3, len(digits))])
}
